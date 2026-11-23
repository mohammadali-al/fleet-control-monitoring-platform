#!/usr/bin/env python3
"""launchkill-sim: test launch/kill controllers against a startup dead time.

Model (all resources in % of the machine: cpu, ram)
  A worker goes: launch -> startup (no usage) -> active (usage ramps up to its
  footprint) -> exits at launch + lifetime, unless killed first.
  Startup is a fixed amount of "startup work" drawn per process (lognormal).
  That work runs slower when the machine is busy and when other workers start
  at the same moment:
      rate = 1 / (1 + lag_load_coef * cpu_load + lag_conc_coef * other_starting)
  So the lag drifts with load, like on a real machine.

Strategies
  greedy            Baseline. 1 Hz aggregate sample, launch into the measured
                    headroom, kill youngest when over.
  reserve_agg       Reservation + feed-forward. Activations are guessed from the
                    1 Hz aggregate only.
  reserve_pid       Reservation + feed-forward. Activations are seen per PID
                    (poll), workers are measured apart from background (cgroup).
  reserve_pid_cull  reserve_pid + over-launch-and-cull + make-before-break
                    handover before each expiry.

What the reservation controllers do (each one was needed, see the sweep):
  - Pending workers count against the ceiling from their earliest possible
    start. Uncertain footprints are pooled: mean + z*sqrt(sum of variances),
    not a per-worker worst case (that over-reserves and forces a 2nd lag).
  - A burst of n is timed by the lag measured at concurrency n-1, and the
    per-worker lag quantile is set so the chance that ANY member starts early
    is lag_q_lo.
  - Earliest start is looked up at today's (easier) load; latest start and
    timeouts at the (harder) load ahead. A timeout records a censored lag.
  - type_order=spread: a wide-lag type waits if a tight-lag type could take
    the same room a moment later.
  - Falling edges are killed on schedule, one poll ahead.

Commands
  python3 sim.py run   [--config my.json] [--strategy all] [--seed 1] [--plot out.png]
  python3 sim.py sweep [--config my.json] [--strategy reserve_pid_cull] [--seeds 8]
                       [--grid '{"margin.cpu":[1,2,3]}'] [--w-over 10] [--top 12]
  python3 sim.py example-config > my.json

Config: a JSON file. Missing keys fall back to the defaults (see example-config).
"types" and "profile" replace the defaults as a whole.
Profile: per resource, a list of [time, value] points, linear in between.
Two points with the same time make an instant step.
"""

import argparse
import copy
import itertools
import json
import os
import sys
from collections import deque
from concurrent.futures import ProcessPoolExecutor
from dataclasses import dataclass
from statistics import NormalDist

import numpy as np

RES = ("cpu", "ram")
STRATEGIES = ("greedy", "reserve_agg", "reserve_pid", "reserve_pid_cull")

DEFAULT_CONFIG = {
    "sim": {"duration": 120.0, "dt": 0.05},
    "machine": {
        "background": {
            "cpu": {"mean": 8.0, "sd": 1.5, "tau": 15.0},
            "ram": {"mean": 20.0, "sd": 0.5, "tau": 30.0},
        },
        "sample_noise": {"cpu": 1.0, "ram": 0.2},
        "pid_noise": 0.03,
        "lag_load_coef": 1.0,
        "lag_conc_coef": 0.08,
    },
    "types": {
        "big": {"cpu": 12.0, "ram": 5.0, "fp_sd": 0.15, "lag_median": 2.5,
                "lag_sigma": 0.3, "lifetime": 60.0, "ramp": 0.4},
        "small": {"cpu": 3.0, "ram": 1.2, "fp_sd": 0.15, "lag_median": 1.2,
                  "lag_sigma": 0.25, "lifetime": 40.0, "ramp": 0.2},
    },
    "profile": {
        "cpu": [[0, 15], [5, 15], [20, 50], [30, 50], [30, 80], [45, 80], [45, 40],
                [55, 40], [55, 75], [56, 75], [56, 40],
                [65, 40], [65, 75], [68, 75], [68, 40],
                [75, 40], [75, 75], [81, 75], [81, 40],
                [90, 40], [90, 20], [100, 20], [100, 60], [120, 60]],
        "ram": [[0, 60], [84, 60], [84, 33], [95, 33], [95, 60], [120, 60]],
    },
    "controller": {
        "margin": {"cpu": 2.0, "ram": 1.0},  # stay this far under the ceiling
        "shed_frac": 0.5,          # shed when within shed_frac*margin of the ceiling
        "lag_q_lo": 0.05,          # chance that ANY member of a burst starts before its window
        "lag_q_hi": 0.95,          # lag quantile used as "latest activation"
        "fp_q_hi": 0.9,            # quantile of the SUM of uncertain footprints that must fit
        "timeout_mult": 1.5,       # pending older than hi*this is treated as late and killed
        "poll": 0.2,               # controller tick and per-PID poll period (s)
        "agg_period": 1.0,         # aggregate sample period (s)
        "detect_frac": 0.3,        # per-PID: active once usage > this * mean footprint
        "infer_frac": 0.6,         # aggregate-only: jump of this * footprint = one activation
        "settle": 1.0,             # charge f_hi for this long after activation (ramp)
        "fb_tau": 10.0,            # background estimate time constant (s)
        "overlaunch": 0.5,         # cull mode: spares per reserved launch
        "max_spares": 6,
        "handover": True,          # cull mode: replace expiring workers make-before-break
        "handover_q": 0.5,         # launch replacement at expiry - (lo + q*(hi-lo))
        "kill_order": "oldest",    # which active workers to kill first: oldest | youngest
        "type_order": "spread",    # which type to try first: size (biggest) | spread (tightest lag)
        "burst_lead": True,        # time a burst of n by the lag at concurrency n-1
        "calib_launches": 12,      # test launches per type and per (load, concurrency) cell
        "calib_loads": [8, 40, 65, 90],   # cpu % at which lag was pre-measured ([] -> isolation only)
        "calib_conc": [0, 1, 4, 8, 12, 20],  # other workers starting at the same time during pre-measurement
        "min_bucket_samples": 8,
        "hi_fallback": 1.5,        # widen hi when a load bucket has too few samples
        "horizon": 15.0,           # look-ahead (s); must exceed the worst lag
    },
}

LOAD_EDGES = np.array([30.0, 55.0, 80.0])   # % cpu buckets for lag learning
CONC_EDGES = np.array([1, 3, 6, 10, 16])    # concurrent-startup buckets


# --------------------------------------------------------------------- config

def deep_merge(base, over):
    for k, v in over.items():
        if isinstance(v, dict) and isinstance(base.get(k), dict):
            deep_merge(base[k], v)
        else:
            base[k] = v
    return base


def load_config(path):
    cfg = copy.deepcopy(DEFAULT_CONFIG)
    if not path:
        return cfg
    with open(path) as f:
        user = json.load(f)
    for k, v in user.items():
        if k in ("types", "profile"):
            cfg[k] = v
        elif isinstance(v, dict) and isinstance(cfg.get(k), dict):
            deep_merge(cfg[k], v)
        else:
            cfg[k] = v
    return cfg


def set_path(cfg, key, value):
    parts = key.split(".")
    node = cfg if parts[0] in cfg else cfg["controller"]
    for p in parts[:-1]:
        node = node[p]
    node[parts[-1]] = value


# -------------------------------------------------------------------- profile

class Profile:
    def __init__(self, prof):
        self.T = {r: np.array([p[0] for p in prof[r]], float) for r in RES}
        self.V = {r: np.array([p[1] for p in prof[r]], float) for r in RES}

    def eval(self, t):
        """r(t) for both resources. Right-continuous at steps."""
        t = np.asarray(t, float)
        out = []
        for r in RES:
            T, V = self.T[r], self.V[r]
            i = np.clip(np.searchsorted(T, t, side="right") - 1, 0, len(T) - 1)
            j = np.minimum(i + 1, len(T) - 1)
            span = T[j] - T[i]
            w = np.where(span > 0, (t - T[i]) / np.where(span > 0, span, 1.0), 0.0)
            w = np.clip(w, 0.0, 1.0)
            out.append(V[i] + w * (V[j] - V[i]))
        return np.array(out)

    def steps(self, res, min_jump=3.0):
        T, V = self.T[res], self.V[res]
        return [(T[k], V[k], V[k + 1]) for k in range(len(T) - 1)
                if T[k + 1] == T[k] and abs(V[k + 1] - V[k]) >= min_jump]


# ------------------------------------------------------------------- machine

class Sim:
    """The real machine. The controller may only use launch/kill/alive/pid_usage/sample."""

    def __init__(self, cfg, seed):
        self.cfg = cfg
        self.m = cfg["machine"]
        self.types = cfg["types"]
        self.rng = np.random.default_rng(seed)
        self.bg = np.array([self.m["background"][r]["mean"] for r in RES], float)
        self.procs = {}
        self.t = 0.0
        self.next_pid = 1
        self.stats = {"launched": 0, "killed_pending": 0, "killed_active": 0, "expired": 0}

    def _footprint(self, typ, n=None):
        T = self.types[typ]
        base = np.array([T[r] for r in RES], float)
        shape = (2,) if n is None else (n, 2)
        return base * np.maximum(0.3, 1 + T["fp_sd"] * self.rng.standard_normal(shape))

    def calibrate(self, typ, n, seed, load_pct, conc):
        """Test launches at a fixed load and concurrency: exact lag and footprint."""
        T = self.types[typ]
        rng = np.random.default_rng(seed)
        work = T["lag_median"] * np.exp(T["lag_sigma"] * rng.standard_normal(n))
        lags = work * (1 + self.m["lag_load_coef"] * load_pct / 100 + self.m["lag_conc_coef"] * conc)
        base = np.array([T[r] for r in RES], float)
        fps = base * np.maximum(0.3, 1 + T["fp_sd"] * rng.standard_normal((n, 2)))
        return lags, fps

    def launch(self, typ):
        T = self.types[typ]
        pid = self.next_pid
        self.next_pid += 1
        self.procs[pid] = {
            "typ": typ, "launch": self.t, "expiry": self.t + T["lifetime"],
            "work": T["lag_median"] * np.exp(T["lag_sigma"] * self.rng.standard_normal()),
            "prog": 0.0, "t_act": None, "fp": self._footprint(typ), "ramp": T["ramp"],
        }
        self.stats["launched"] += 1
        return pid

    def kill(self, pid):
        p = self.procs.pop(pid, None)
        if p is not None:
            self.stats["killed_active" if p["t_act"] is not None else "killed_pending"] += 1

    def alive(self, pid):
        return pid in self.procs

    def _usage(self, p):
        if p["t_act"] is None:
            return np.zeros(2)
        f = min(1.0, (self.t - p["t_act"]) / p["ramp"]) if p["ramp"] > 0 else 1.0
        return p["fp"] * f

    def workers(self):
        u = np.zeros(2)
        for p in self.procs.values():
            u += self._usage(p)
        return u

    def total(self):
        return self.bg + self.workers()

    def pid_usage(self, pid):
        p = self.procs.get(pid)
        if p is None:
            return None
        return self._usage(p) * (1 + self.m["pid_noise"] * self.rng.standard_normal(2))

    def sample(self):
        """(aggregate with noise, worker cgroup total)."""
        noise = np.array([self.m["sample_noise"][r] for r in RES])
        w = self.workers()
        return self.bg + w + noise * self.rng.standard_normal(2), w

    def step(self, dt, t_next):
        for i, r in enumerate(RES):
            b = self.m["background"][r]
            self.bg[i] += ((b["mean"] - self.bg[i]) * dt / b["tau"]
                           + b["sd"] * np.sqrt(2 * dt / b["tau"]) * self.rng.standard_normal())
        load = min(1.0, max(0.0, self.total()[0] / 100))
        n_start = sum(1 for p in self.procs.values() if p["t_act"] is None)
        rate = 1.0 / (1 + self.m["lag_load_coef"] * load
                      + self.m["lag_conc_coef"] * max(0, n_start - 1))
        self.t = t_next
        for pid, p in list(self.procs.items()):
            if self.t >= p["expiry"]:
                del self.procs[pid]
                self.stats["expired"] += 1
            elif p["t_act"] is None:
                p["prog"] += dt * rate
                if p["prog"] >= p["work"]:
                    p["t_act"] = self.t


# ---------------------------------------------------------------- controller

@dataclass
class CP:
    pid: int
    typ: str
    launch: float
    expiry: float
    lo: float
    hi: float
    key: tuple
    state: str = "pending"
    spare: bool = False
    handover_for: int | None = None
    start_after: float = 0.0
    t_active: float | None = None
    usage: np.ndarray | None = None


class Plan:
    """Committed load C(tau) against allowed load R(tau) on a look-ahead grid."""

    def __init__(self, ctl, now):
        g = ctl.c["poll"]
        K = int(ctl.c["horizon"] / g) + 1
        self.now, self.g = now, g
        self.grid = now + g * np.arange(K)
        self.R = ctl.profile.eval(self.grid) - ctl.margin[:, None]
        self.dec = np.zeros(K, bool)
        self.dec[1:] = np.any(self.R[:, 1:] < self.R[:, :-1] - 1e-9, axis=0)
        self.z = ctl.z
        self.C = np.repeat(ctl.bhat[:, None], K, axis=1).astype(float)   # mean
        self.V = np.zeros_like(self.C)                                    # variance
        for p in ctl.procs.values():
            if p.state == "active":
                self.add(-np.inf, p.expiry, *ctl.usage_est(p, now))
            elif not p.spare:
                self.add(ctl.start_of(p), p.expiry, ctl.fmean[p.typ], ctl.fvar[p.typ])

    def add(self, start, end, mean, var):
        m = ((self.grid >= start) & (self.grid < end))[None, :]
        self.C += mean[:, None] * m
        self.V += var[:, None] * m

    def fits(self, start, latest, mean, var):
        """Fits from its earliest activation until its latest one, or until r(t)
        starts to fall (from then on, shedding takes care of it)."""
        K = len(self.grid)
        k0 = int(np.floor((start - self.now) / self.g + 1e-9))
        if k0 >= K:
            return False
        k0 = max(k0, 0)
        k1 = max(k0, min(int(np.ceil((latest - self.now) / self.g)), K - 1))
        d = np.nonzero(self.dec[k0 + 1:k1 + 1])[0]
        if d.size:
            k1 = k0 + d[0]
        w = slice(k0, k1 + 1)
        need = self.C[:, w] + mean[:, None] + self.z * np.sqrt(self.V[:, w] + var[:, None])
        return bool(np.all(need <= self.R[:, w]))


class Controller:
    def __init__(self, cfg, strategy, sim, seed):
        self.c = c = cfg["controller"]
        self.strategy = strategy
        self.observe = "agg" if strategy in ("greedy", "reserve_agg") else "pid"
        self.cull = strategy == "reserve_pid_cull"
        self.handover = self.cull and c["handover"]
        self.types = cfg["types"]
        self.profile = Profile(cfg["profile"])
        self.margin = np.array([c["margin"][r] for r in RES], float)
        self.procs = {}
        self.bhat = None
        self.spare_credit = 0.0
        self.handed_over = set()
        self.stats = {"culled": 0, "timeouts": 0}

        # Pre-measurement: test-launch each type in isolation (background load),
        # plus at the extra load levels / concurrency given in the config.
        self.fmean, self.fvar, self.calib_med = {}, {}, {}
        self.z = NormalDist().inv_cdf(c["fp_q_hi"])
        self.lag, self._qcache = {}, {}
        bg = cfg["machine"]["background"]["cpu"]["mean"]
        cells = sorted({(bg, 0)} | {(L, n) for L in c["calib_loads"] for n in c["calib_conc"]})
        self.calib_spread = calib_spread = {}
        for k, typ in enumerate(self.types):
            fps_all = []
            for j, (L, n) in enumerate(cells):
                lags, fps = sim.calibrate(typ, c["calib_launches"], seed * 1000 + k * 100 + j, L, n)
                fps_all.append(fps)
                key = (typ, self.lbucket(L), self.cbucket(n))
                self.lag.setdefault(key, deque(maxlen=200)).extend(lags.tolist())
                if (L, n) == (bg, 0):
                    self.calib_med[typ] = float(np.median(lags))
                    calib_spread[typ] = float(np.quantile(lags, c["lag_q_hi"])
                                              - np.quantile(lags, c["lag_q_lo"]))
            fps = np.vstack(fps_all)
            self.fmean[typ] = fps.mean(axis=0)
            self.fvar[typ] = fps.var(axis=0)

        if c["type_order"] == "spread":
            self.type_order = sorted(self.types, key=lambda t: calib_spread[t])
        else:
            self.type_order = sorted(self.types, key=lambda t: -self.fmean[t][0])

    # ---- helpers
    @staticmethod
    def lbucket(load_pct):
        return int(np.searchsorted(LOAD_EDGES, load_pct, side="right"))

    @staticmethod
    def cbucket(n):
        return int(np.searchsorted(CONC_EDGES, n, side="right"))

    def start_of(self, p):
        return max(p.launch + p.lo, p.start_after)

    def usage_est(self, p, now):
        """(mean, variance) of an active worker's usage. Uncertain until it has
        been measured after its ramp; exact (per-PID) after that."""
        if self.observe == "agg" or p.usage is None:
            return self.fmean[p.typ], self.fvar[p.typ]
        if now - p.t_active < self.c["settle"]:
            return np.maximum(p.usage, self.fmean[p.typ]), self.fvar[p.typ]
        return p.usage, np.zeros(2)

    def est_active_now(self, now, exclude=None, with_var=False):
        u, v = self.bhat.copy(), np.zeros(2)
        for p in self.procs.values():
            if p.state == "active" and p is not exclude:
                m, var = self.usage_est(p, now)
                u += m
                v += var
        return (u, v) if with_var else u

    def pending(self, spare=None):
        return [p for p in self.procs.values() if p.state == "pending"
                and (spare is None or p.spare == spare)]

    def _bucket_q(self, typ, lb, cb, q, easier):
        ck = (typ, lb, cb, q, easier)
        if ck in self._qcache:
            return self._qcache[ck]
        s = self.lag.get((typ, lb, cb))
        widen = False
        if s is not None and len(s) >= self.c["min_bucket_samples"]:
            pool = s
        else:
            # Lag only grows with load and concurrency. So for the earliest
            # activation, samples from same-or-easier buckets are a safe lower
            # bound; for the latest, same-or-harder buckets are a safe upper one.
            pool = [x for (t, l, cc), v in self.lag.items() if t == typ
                    and ((l <= lb and cc <= cb) if easier else (l >= lb and cc >= cb)) for x in v]
            if len(pool) < self.c["min_bucket_samples"]:
                pool = [x for (t, _, _), v in self.lag.items() if t == typ for x in v]
                widen = not easier
        v = float(np.quantile(np.fromiter(pool, float), q)) * (self.c["hi_fallback"] if widen else 1)
        self._qcache[ck] = v
        return v

    def lag_bounds(self, typ, now, conc=None, est=None, group=1):
        """Earliest activation uses the easier condition (load now): too short is
        safe, too long would let it start before the edge. Latest activation and
        the timeout use the harder one (the load it is heading into)."""
        c = self.c
        if est is None:
            est = self.est_active_now(now)[0]
        if conc is None:
            conc = len(self.pending())
        tgt = self.profile.eval(now + self.calib_med[typ])[0] - self.margin[0]
        cb = self.cbucket(conc)
        key = (typ, self.lbucket(est), cb)
        q_lo = 1 - (1 - c["lag_q_lo"]) ** (1 / max(1, group))   # per-member quantile
        lo = self._bucket_q(typ, key[1], cb, q_lo, True)
        hi = self._bucket_q(typ, self.lbucket(max(est, tgt)), cb, c["lag_q_hi"], False)
        return lo, max(hi, lo), key

    def record_lag(self, key, value):
        self.lag.setdefault(key, deque(maxlen=200)).append(max(0.01, value))
        self._qcache.clear()

    def launch(self, sim, now, typ, lo, hi, key, spare=False, handover_for=None, start_after=0.0):
        pid = sim.launch(typ)
        self.procs[pid] = CP(pid, typ, now, now + self.types[typ]["lifetime"], lo, hi, key,
                             spare=spare, handover_for=handover_for, start_after=start_after)

    def kill(self, sim, p):
        sim.kill(p.pid)
        self.procs.pop(p.pid, None)

    # ---- the tick
    def tick(self, now, sim, sample):
        for pid in [pid for pid in self.procs if not sim.alive(pid)]:
            del self.procs[pid]
        if self.bhat is None:
            if sample is None:
                return
            self.bhat = sample[0] - sample[1]
        if self.strategy == "greedy":
            return self.tick_greedy(now, sim, sample)

        c = self.c
        if self.observe == "pid":
            self.observe_pid(now, sim)
        if sample is not None:
            self.observe_sample(now, sim, sample)

        # Late is not failed: hold the reservation until hi*timeout, then give up.
        for p in self.pending():
            if now - p.launch > p.hi * c["timeout_mult"]:
                self.record_lag(p.key, now - p.launch)   # censored sample pushes hi up
                self.kill(sim, p)
                self.stats["timeouts"] += 1

        self.shed(now, sim)

        if self.cull and not self.pending(spare=False):
            for p in self.pending(spare=True):
                self.kill(sim, p)
            self.spare_credit = 0.0

        plan = Plan(self, now)
        if self.handover:
            self.launch_handovers(now, sim, plan)
        self.launch_reserved(now, sim, plan)

    def observe_pid(self, now, sim):
        for p in list(self.procs.values()):
            u = sim.pid_usage(p.pid)
            if u is None:
                self.procs.pop(p.pid, None)
                continue
            if p.state == "pending":
                if np.any(u > self.c["detect_frac"] * self.fmean[p.typ]):
                    self.on_active(now, sim, p, u)
            elif now - p.t_active < self.c["settle"]:
                p.usage = u
            else:
                p.usage = 0.8 * p.usage + 0.2 * u

    def observe_sample(self, now, sim, sample):
        total, workers = sample
        a = self.c["agg_period"] / self.c["fb_tau"]
        if self.observe == "pid":
            # cgroup: background = aggregate - workers, no ambiguity.
            self.bhat += a * ((total - workers) - self.bhat)
            return
        # Aggregate only: an unexplained jump means the oldest pending one started.
        excess = total - self.est_active_now(now)
        for p in sorted(self.pending(), key=lambda q: q.launch):
            f = self.fmean[p.typ]
            if now - p.launch < p.lo or excess[0] < self.c["infer_frac"] * f[0]:
                break
            self.on_active(now, sim, p, f)
            excess -= f
        # Feedback only when nothing may be starting (else a jump is ambiguous).
        if not any(now - p.launch >= p.lo for p in self.pending()):
            resid = total - (self.est_active_now(now) - self.bhat)
            self.bhat += a * (resid - self.bhat)

    def on_active(self, now, sim, p, u):
        p.state, p.t_active, p.usage = "active", now, u
        detect_delay = self.c["poll"] if self.observe == "pid" else self.c["agg_period"]
        self.record_lag(p.key, now - p.launch - detect_delay)   # earliest consistent start
        if p.handover_for is not None:
            old = self.procs.get(p.handover_for)
            if old is not None:
                self.kill(sim, old)
        if not self.cull:
            return
        room = self.profile.eval(now) - self.c["shed_frac"] * self.margin \
            - self.est_active_now(now, exclude=p)
        if np.any(np.maximum(u, self.fmean[p.typ]) > room):
            self.kill(sim, p)
            self.stats["culled"] += 1
            return
        if p.spare:
            # The spare took a slot: release the youngest reservation still pending.
            p.spare = False
            res = [q for q in self.pending(spare=False) if q.handover_for is None]
            same = [q for q in res if q.typ == p.typ]
            victims = same or res
            if victims:
                self.kill(sim, max(victims, key=lambda q: q.launch))

    def shed(self, now, sim):
        """Kill on schedule: look one tick ahead so falling edges are met on time."""
        c = self.c
        R1 = np.minimum(self.profile.eval(now), self.profile.eval(now + c["poll"]))
        thr = R1 - c["shed_frac"] * self.margin
        target = R1 - self.margin

        est = self.est_active_now(now)
        if np.any(est > thr):
            active = [p for p in self.procs.values() if p.state == "active"]
            active.sort(key=lambda q: q.launch, reverse=(c["kill_order"] == "youngest"))
            for p in active:
                viol = est > target
                if not np.any(viol):
                    break
                u = self.usage_est(p, now)[0]
                if np.any(u[viol] > 0):
                    self.kill(sim, p)
                    est -= u

        # Pending ones that could start before the next tick must fit too.
        soon = [p for p in self.pending(spare=False) if self.start_of(p) <= now + c["poll"]]
        mean = est + sum((self.fmean[p.typ] for p in soon), np.zeros(2))
        var = self.est_active_now(now, with_var=True)[1] + sum((self.fvar[p.typ] for p in soon), np.zeros(2))
        if np.any(mean + self.z * np.sqrt(var) > thr):
            for p in sorted(soon, key=lambda q: q.launch, reverse=True):   # youngest first
                if np.all(mean + self.z * np.sqrt(var) <= target):
                    break
                self.kill(sim, p)
                mean -= self.fmean[p.typ]
                var -= self.fvar[p.typ]

    def launch_handovers(self, now, sim, plan):
        for X in sorted((p for p in self.procs.values() if p.state == "active"),
                        key=lambda q: q.expiry):
            if X.pid in self.handed_over:
                continue
            lo, hi, key = self.lag_bounds(X.typ, now)
            lead = lo + self.c["handover_q"] * (hi - lo)
            left = X.expiry - now
            if left > lead:
                break
            if left < lo:
                continue        # too late for make-before-break; the normal rule refills
            start = max(now + lo, X.expiry)
            fm, fv = self.fmean[X.typ], self.fvar[X.typ]
            if plan.fits(start, max(start, now + hi), fm, fv):
                self.launch(sim, now, X.typ, lo, hi, key, handover_for=X.pid, start_after=X.expiry)
                plan.add(start, now + self.types[X.typ]["lifetime"], fm, fv)
                self.handed_over.add(X.pid)

    def best_burst(self, plan, typ, now, n_pend, bounds):
        """Largest n such that n workers launched together now fit. Members of
        a burst slow each other down, so a burst of n is timed by the lag at
        concurrency n-1: even its fastest member starts with n-1 others."""
        fm, fv = self.fmean[typ], self.fvar[typ]
        room = np.maximum(np.max(plan.R - plan.C, axis=1), 0)
        n_max = int(min(32, np.min(np.floor(room / np.maximum(fm, 1e-9)))))
        extra = self.c["overlaunch"] if self.cull else 0.0   # spares start alongside
        best = None
        for n in range(1, n_max + 1):
            m = int(n * (1 + extra))
            lo, hi, key = bounds(typ, n_pend + m - 1, m)
            if plan.fits(now + lo, now + hi, n * fm, n * fv):
                best = (n, lo, hi, key)
        return best

    def launch_reserved(self, now, sim, plan):
        c = self.c
        est = self.est_active_now(now)[0]
        cache = {}

        def bounds(typ, conc, group=1):
            k = (typ, self.cbucket(conc), group)
            if k not in cache:
                cache[k] = self.lag_bounds(typ, now, conc=conc, est=est, group=group)
            return cache[k]

        for _ in range(64):
            n_pend = len(self.pending())
            choice, tried = None, []
            for typ in self.type_order:
                fm, fv = self.fmean[typ], self.fvar[typ]
                if c["burst_lead"]:
                    b = self.best_burst(plan, typ, now, n_pend, bounds)
                else:
                    lo, hi, key = bounds(typ, n_pend)
                    b = (1, lo, hi, key) if plan.fits(now + lo, now + hi, fm, fv) else None
                if b is None:
                    tried.append(typ)
                    continue
                n, lo, hi, key = b
                # spread mode: if a preferred (tighter-lag) type would fit this
                # same room when it can start, leave the room to it.
                if c["type_order"] == "spread" and any(
                        plan.fits(now + lo, now + lo + self.calib_spread[t], self.fmean[t], self.fvar[t])
                        for t in tried):
                    break
                choice = (typ, n, lo, hi, key)
                break
            if choice is None:
                return
            typ, n, lo, hi, key = choice
            for _ in range(n):
                self.launch(sim, now, typ, lo, hi, key)
                plan.add(now + lo, now + self.types[typ]["lifetime"], self.fmean[typ], self.fvar[typ])
            if self.cull:
                self.spare_credit += c["overlaunch"] * n
                while self.spare_credit >= 1 and len(self.pending(spare=True)) < c["max_spares"]:
                    self.launch(sim, now, typ, lo, hi, key, spare=True)
                    self.spare_credit -= 1

    def tick_greedy(self, now, sim, sample):
        """What most people write first: pure feedback on the 1 Hz aggregate."""
        if sample is None:
            return
        total = sample[0]
        Rn = self.profile.eval(now)
        excess = total - Rn
        if np.any(excess > 0):
            for p in sorted(self.procs.values(), key=lambda q: q.launch, reverse=True):
                if np.all(excess <= 0):
                    break
                self.kill(sim, p)
                excess -= self.fmean[p.typ]
            return
        room = Rn - self.margin - total
        for typ in self.type_order:
            while np.all(self.fmean[typ] <= room):
                self.launch(sim, now, typ, 0.0, 99.0, (typ, 0, 0))
                room -= self.fmean[typ]


# ----------------------------------------------------------------- run + metrics

def run_one(cfg, strategy, seed, keep_trace=False):
    sim = Sim(cfg, seed)
    ctl = Controller(cfg, strategy, sim, seed)
    dt, T = cfg["sim"]["dt"], cfg["sim"]["duration"]
    n = int(round(T / dt))
    ctl_every = max(1, int(round(ctl.c["poll"] / dt)))
    smp_every = max(1, int(round(ctl.c["agg_period"] / dt)))
    y = np.zeros((2, n))
    for i in range(n):
        now = i * dt
        sample = sim.sample() if i % smp_every == 0 else None
        if i % ctl_every == 0 or sample is not None:
            ctl.tick(now, sim, sample)
        sim.step(dt, (i + 1) * dt)
        y[:, i] = sim.total()
    t = (np.arange(n) + 1) * dt
    r = ctl.profile.eval(t)
    m = metrics(t, y, r, ctl.profile, dt, T)
    m.update(launched=sim.stats["launched"], wasted=sim.stats["killed_pending"],
             culled=ctl.stats["culled"], timeouts=ctl.stats["timeouts"])
    return m, ({"t": t, "y": y, "r": r} if keep_trace else None)


def metrics(t, y, r, profile, dt, T, tol=0.5):
    over = np.clip(y - r, 0, None)
    deficit = np.clip(np.min(r - y, axis=0), 0, None)   # gap of the closest-to-ceiling resource
    m = {
        "over_cpu": over[0].sum() * dt,
        "over_ram": over[1].sum() * dt,
        "max_over": float(over.max()),
        "t_over": float(np.any(over > tol, axis=0).sum() * dt),
        "under": float(deficit.sum() * dt),
        "edges": [],
    }
    for ri, res in enumerate(RES):
        steps = profile.steps(res)
        for k, (ts, before, after) in enumerate(steps):
            t_next = steps[k + 1][0] if k + 1 < len(steps) else T
            win = (t >= ts) & (t < t_next)
            tt, yy = t[win], y[ri, win]
            d = after - before
            e = {"res": res, "t": ts, "from": before, "to": after}
            if d > 0:
                late = win & (t >= ts + 0.5 * (t_next - ts))
                gap_self = np.mean(r[ri, late] - y[ri, late])
                gap_other = np.mean(r[1 - ri, late] - y[1 - ri, late])
                e["other_binds"] = bool(gap_other < 8.0 and gap_other < 0.5 * gap_self)
                hit = np.nonzero(yy >= after - max(5.0, 0.1 * d))[0]
                e["t90"] = float(tt[hit[0]] - ts) if hit.size else None
                e["fill"] = (float(np.mean(np.clip((yy - before) / d, 0, 1)) * 100)
                             if t_next - ts <= 10 else None)
            else:
                ok = np.nonzero(yy <= after + tol)[0]
                e["settle"] = float(tt[ok[0]] - ts) if ok.size else None
            m["edges"].append(e)
    return m


def average(ms):
    """Mean of the scalar metrics over seeds; medians for edge times (never = inf)."""
    keys = ("over_cpu", "over_ram", "max_over", "t_over", "under", "launched", "wasted", "culled")
    out = {k: float(np.mean([m[k] for m in ms])) for k in keys}
    out["worst_over"] = float(max(m["max_over"] for m in ms))
    out["edges"] = []
    for k, e0 in enumerate(ms[0]["edges"]):
        es = [m["edges"][k] for m in ms]
        e = {x: e0[x] for x in ("res", "t", "from", "to")}
        if "settle" in e0:
            e["settle"] = float(np.median([x["settle"] if x["settle"] is not None else np.inf for x in es]))
        else:
            e["other_binds"] = sum(x["other_binds"] for x in es) > len(es) / 2
            e["t90"] = float(np.median([x["t90"] if x["t90"] is not None else np.inf for x in es]))
            e["fill"] = None if e0["fill"] is None else float(np.mean([x["fill"] for x in es]))
        out["edges"].append(e)
    return out


def cost(m, w_over):
    return w_over * (m["over_cpu"] + m["over_ram"]) + m["under"]


# ------------------------------------------------------------------- output

def fmt(v, spec="{:.1f}", none="never"):
    return none if v is None or not np.isfinite(v) else spec.format(v)


def print_run(results, w_over, n_seeds):
    if n_seeds > 1:
        print(f"mean over {n_seeds} seeds (edge times: median)\n")
    print(f"{'strategy':<18}{'over cpu':>9}{'over ram':>9}{'max over':>9}{'worst':>7}{'t>ceil':>8}"
          f"{'under':>8}{'cost':>8}{'launch':>7}{'wasted':>7}{'culled':>7}")
    print(f"{'':<18}{'%·s':>9}{'%·s':>9}{'%':>9}{'%':>7}{'s':>8}{'%·s':>8}")
    for name, m in results.items():
        print(f"{name:<18}{m['over_cpu']:>9.1f}{m['over_ram']:>9.1f}{m['max_over']:>9.1f}"
              f"{m['worst_over']:>7.1f}{m['t_over']:>8.1f}{m['under']:>8.0f}{cost(m, w_over):>8.0f}"
              f"{m['launched']:>7.0f}{m['wasted']:>7.0f}{m['culled']:>7.0f}")
    print("\nEdges  rise: time until within max(5, 10% of step) of the new ceiling"
          " [/ mean fill for pulses <= 10 s]\n       fall: time until under the new ceiling")
    names = list(results)
    print(f"{'edge':<24}" + "".join(f"{n:>18}" for n in names))
    for k, e in enumerate(next(iter(results.values()))["edges"]):
        label = f"{e['res']} {e['t']:>5.0f}s {e['from']:.0f}->{e['to']:.0f}"
        cells = []
        for n in names:
            x = results[n]["edges"][k]
            if "settle" in x:
                cells.append(f"fall {fmt(x['settle'], '{:.1f}s')}")
            elif x["other_binds"]:
                cells.append("other res binds")
            elif x["fill"] is not None:
                cells.append(f"{fmt(x['t90'], '{:.1f}s')} / {x['fill']:.0f}%")
            else:
                cells.append(f"rise {fmt(x['t90'], '{:.1f}s')}")
        print(f"{label:<24}" + "".join(f"{c:>18}" for c in cells))


def plot(traces, path):
    import matplotlib
    matplotlib.use("Agg")
    import matplotlib.pyplot as plt
    SURF, INK, INK2, GRID = "#fcfcfb", "#0b0b0b", "#52514e", "#e4e3df"
    SERIES, CRIT = "#2a78d6", "#d03b3b"
    n = len(traces)
    fig, axes = plt.subplots(n, 2, figsize=(15, 2.3 * n + 0.9), sharex=True,
                             squeeze=False, facecolor=SURF)
    for row, (name, tr) in enumerate(traces.items()):
        for col, res in enumerate(RES):
            ax = axes[row][col]
            ax.set_facecolor(SURF)
            t, y, r = tr["t"], tr["y"][col], tr["r"][col]
            ax.plot(t, r, color=INK2, lw=1.2, ls="--", label="ceiling r(t)")
            ax.plot(t, y, color=SERIES, lw=1.4, label="actual usage")
            ax.fill_between(t, r, y, where=y > r, color=CRIT, alpha=0.4, lw=0,
                            interpolate=True, label="over ceiling")
            ax.grid(axis="y", color=GRID, lw=0.8)
            ax.set_axisbelow(True)
            for s in ("top", "right"):
                ax.spines[s].set_visible(False)
            for s in ("left", "bottom"):
                ax.spines[s].set_color(GRID)
            ax.tick_params(colors=INK2, labelsize=8)
            ax.set_title(f"{name}  ·  {res} %", loc="left", fontsize=9.5, color=INK)
    for col in range(2):
        axes[-1][col].set_xlabel("time (s)", color=INK2, fontsize=9)
    h, l = axes[0][0].get_legend_handles_labels()
    fig.legend(h, l, loc="upper center", ncol=3, frameon=False, fontsize=9, labelcolor=INK)
    fig.tight_layout(rect=(0, 0, 1, 1 - 0.35 / (2.3 * n + 0.9)))
    fig.savefig(path, dpi=130, facecolor=SURF)


# -------------------------------------------------------------------- sweep

def _sweep_task(args):
    cfg, strategy, seed, w_over = args
    m, _ = run_one(cfg, strategy, seed)
    return {k: m[k] for k in ("over_cpu", "over_ram", "max_over", "under", "launched", "wasted")} \
        | {"cost": cost(m, w_over)}


def sweep(cfg, strategy, seeds, grid, top, w_over, jobs):
    keys = list(grid)
    combos = list(itertools.product(*grid.values()))
    tasks = []
    for combo in combos:
        cc = copy.deepcopy(cfg)
        for k, v in zip(keys, combo):
            set_path(cc, k, v)
        tasks += [(cc, strategy, s, w_over) for s in seeds]
    print(f"{len(combos)} configs x {len(seeds)} seeds = {len(tasks)} runs of {strategy}"
          f" on {jobs} workers ...", file=sys.stderr)
    with ProcessPoolExecutor(jobs) as ex:
        res = list(ex.map(_sweep_task, tasks, chunksize=2))
    rows = []
    S = len(seeds)
    for i, combo in enumerate(combos):
        rr = res[i * S:(i + 1) * S]
        agg = {k: float(np.mean([x[k] for x in rr])) for k in rr[0]}
        agg["cost_sd"] = float(np.std([x["cost"] for x in rr]))
        agg["worst_max_over"] = float(np.max([x["max_over"] for x in rr]))
        rows.append((combo, agg))
    rows.sort(key=lambda x: x[1]["cost"])
    head = "".join(f"{k:>14}" for k in keys)
    print(head + f"{'cost':>9}{'±sd':>7}{'over %·s':>10}{'max over':>9}{'worst':>7}"
          f"{'under':>8}{'launch':>8}{'wasted':>8}")
    for combo, a in rows[:top]:
        print("".join(f"{str(v):>14}" for v in combo)
              + f"{a['cost']:>9.0f}{a['cost_sd']:>7.0f}{a['over_cpu'] + a['over_ram']:>10.1f}"
                f"{a['max_over']:>9.1f}{a['worst_max_over']:>7.1f}{a['under']:>8.0f}"
                f"{a['launched']:>8.0f}{a['wasted']:>8.0f}")


# ---------------------------------------------------------------------- CLI

def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest="cmd", required=True)
    pr = sub.add_parser("run", help="run strategies once and compare")
    pr.add_argument("--config")
    pr.add_argument("--strategy", default="all", help="all or a comma list of: " + ", ".join(STRATEGIES))
    pr.add_argument("--seed", type=int, default=1, help="first seed")
    pr.add_argument("--seeds", type=int, default=1, help="average over this many seeds")
    pr.add_argument("--plot", help="write a PNG of usage vs ceiling")
    pr.add_argument("--w-over", type=float, default=10.0, help="cost weight of overshoot vs underfill")
    ps = sub.add_parser("sweep", help="grid-search controller settings over many seeds")
    ps.add_argument("--config")
    ps.add_argument("--strategy", default="reserve_pid_cull", choices=STRATEGIES)
    ps.add_argument("--seeds", type=int, default=8)
    ps.add_argument("--grid", help='JSON, e.g. {"margin.cpu":[1,2,3],"lag_q_lo":[0.01,0.05]}')
    ps.add_argument("--top", type=int, default=12)
    ps.add_argument("--w-over", type=float, default=10.0)
    ps.add_argument("--jobs", type=int, default=os.cpu_count())
    sub.add_parser("example-config", help="print the default config as JSON")
    a = ap.parse_args()

    if a.cmd == "example-config":
        json.dump(DEFAULT_CONFIG, sys.stdout, indent=2)
        print()
        return
    cfg = load_config(a.config)
    if a.cmd == "run":
        names = STRATEGIES if a.strategy == "all" else tuple(a.strategy.split(","))
        results, traces = {}, {}
        for name in names:
            ms = []
            for k in range(a.seeds):
                m, tr = run_one(cfg, name, a.seed + k, keep_trace=bool(a.plot) and k == 0)
                ms.append(m)
                if k == 0:
                    traces[name] = tr
            results[name] = average(ms)
        print_run(results, a.w_over, a.seeds)
        if a.plot and a.seeds > 1:
            print(f"\n(plot shows seed {a.seed})")
        if a.plot:
            plot(traces, a.plot)
            print(f"\nplot: {a.plot}")
    else:
        if a.grid:
            grid = json.loads(a.grid)
        else:
            grid = {"margin.cpu": [0.5, 1.0, 2.0], "lag_q_lo": [0.02, 0.05, 0.15]}
            if a.strategy == "reserve_pid_cull":
                grid["overlaunch"] = [0.0, 0.5, 1.0]
        sweep(cfg, a.strategy, list(range(1, a.seeds + 1)), grid, a.top, a.w_over, a.jobs)


if __name__ == "__main__":
    main()

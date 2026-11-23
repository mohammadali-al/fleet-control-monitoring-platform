import sys, copy, itertools, numpy as np
sys.path.insert(0, '/home/bierellow/launchkill-sim')
import sim as S
from concurrent.futures import ProcessPoolExecutor

def job(args):
    strat, ol, q, cc, seed = args
    cfg = S.load_config(None)
    cfg["controller"]["overlaunch"] = ol
    cfg["controller"]["lag_q_lo"] = q
    cfg["machine"]["lag_conc_coef"] = cc
    m, _ = S.run_one(cfg, strat, seed)
    e = {(x["res"], x["t"]): x for x in m["edges"]}
    r = lambda t: e[("cpu", t)]["t90"] if e[("cpu", t)]["t90"] is not None else np.inf
    return dict(over=m["over_cpu"] + m["over_ram"], worst=m["max_over"], under=m["under"],
                wasted=m["wasted"], launched=m["launched"],
                r30=r(30), r100=r(100),
                p1=e[("cpu", 55)]["fill"], p3=e[("cpu", 65)]["fill"], p6=e[("cpu", 75)]["fill"])

if __name__ == "__main__":
    seeds = range(1, 9)
    rows = [("reserve_pid", 0.0)] + [("reserve_pid_cull", ol) for ol in (0.0, 0.25, 0.5, 1.0, 2.0)]
    combos = [(s, ol, q, cc) for cc in (0.0, 0.08, 0.2) for q in (0.05, 0.25) for s, ol in rows]
    tasks = [c + (sd,) for c in combos for sd in seeds]
    with ProcessPoolExecutor() as ex:
        res = list(ex.map(job, tasks, chunksize=2))
    k = len(seeds)
    print(f"{'conc':>5}{'q_lo':>6} {'strategy':<17}{'m/n':>5}{'over':>7}{'worst':>7}{'under':>7}{'rise30':>8}{'rise100':>8}{'fill1s':>7}{'fill3s':>7}{'fill6s':>7}{'wasted':>7}")
    for i, (s, ol, q, cc) in enumerate(combos):
        rr = res[i*k:(i+1)*k]
        mean = lambda key: np.mean([x[key] for x in rr])
        med = lambda key: np.median([x[key] for x in rr])
        name = "base (no cull)" if s == "reserve_pid" else "cull"
        print(f"{cc:>5}{q:>6} {name:<17}{ol:>5}{mean('over'):>7.1f}{max(x['worst'] for x in rr):>7.1f}{mean('under'):>7.0f}"
              f"{med('r30'):>8.1f}{med('r100'):>8.1f}{mean('p1'):>7.0f}{mean('p3'):>7.0f}{mean('p6'):>7.0f}{mean('wasted'):>7.0f}")
        if i % len(rows) == len(rows) - 1: print()

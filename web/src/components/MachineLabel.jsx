/**
 * Consistent machine identity: `name · private-IP · public-IP`.
 * The private IP disambiguates machines that share a username and a public IP behind NAT.
 * Missing parts are omitted (no dangling separators).
 *   variant="full"    name + IPs as mono chips
 *   variant="compact" name + IPs as muted inline mono text
 * `machine` may be a machine object, or pass `name` explicitly (e.g. when only an id is known).
 */
export function primaryPrivateIP(machine) {
  return machine?.primary_private_ip || machine?.private_ips?.[0] || '';
}

export function MachineLabel({ machine, name, variant = 'full' }) {
  const label = name || machine?.name || machine?.id || '';
  const priv = primaryPrivateIP(machine);
  const pub = machine?.public_ip || '';
  const ips = [priv, pub].filter(Boolean);
  const compact = variant === 'compact';

  return (
    <span className="machine-label" style={{ display: 'inline-flex', alignItems: 'baseline', flexWrap: 'wrap', gap: 'var(--sp-1)' }}>
      <span className="mono">{label}</span>
      {ips.map((ip) =>
        compact ? (
          <span key={ip} className="faint mono">
            {`· ${ip}`}
          </span>
        ) : (
          <span key={ip} className="chip chip--outline chip--mono">
            {ip}
          </span>
        )
      )}
    </span>
  );
}

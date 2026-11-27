/**
 * Inline status alert matching the panel's `[data-form-status]` pattern.
 * `status` is `{ kind: 'info'|'ok'|'warn'|'danger', msg: string }` or null.
 */
export function StatusAlert({ status, id, style }) {
  const hasMsg = Boolean(status && status.msg);
  const kind = status?.kind || 'info';
  const cls = kind === 'info' ? 'alert' : `alert alert--${kind}`;
  const iconId = kind === 'ok' ? 'i-check' : kind === 'danger' || kind === 'warn' ? 'i-alert' : 'i-info';

  return (
    <div
      className={cls}
      id={id}
      data-form-status
      role="status"
      aria-live="polite"
      hidden={!hasMsg}
      style={style}
    >
      {hasMsg && (
        <>
          <svg className="icon" aria-hidden="true">
            <use href={`#${iconId}`} />
          </svg>
          <div>{status.msg}</div>
        </>
      )}
    </div>
  );
}

import { usePageTitle } from '../lib/title';

export function MachinesPage() {
  usePageTitle('Machines');
  return (
    <div className="page-head">
      <div className="page-head__text">
        <h1>Machines</h1>
        <p>Loading registry...</p>
      </div>
    </div>
  );
}

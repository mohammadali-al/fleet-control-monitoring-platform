
## T-0070 log
Agent reports primary private IP (default-route source IP via capture.DefaultRouteSourceIP) as net-info envelope on connect and on change (30s re-check), worker + monitor; fleet.recordNetInfo validates and stores registry.Machine.PrimaryPrivateIP; MachinesPage shows Private/Public in row and details; bundle rebuilt. Tests: TestAgentReportsPrimaryPrivateIPOnConnect, TestNetInfoDispatch, machines_test.

<!-- wf: commits=pending left=- tests=server:pass,agent:pass blocked=no -->
wf: T-0070 done

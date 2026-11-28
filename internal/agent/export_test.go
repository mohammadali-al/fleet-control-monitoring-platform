package agent

import "time"

// SetEvidencePushInterval changes how often an idle worker offers unsynced
// segments (the default is a minute) and returns the function that restores it.
// Set it before the agent starts.
func SetEvidencePushInterval(d time.Duration) (restore func()) {
	old := evidencePushInterval
	evidencePushInterval = d
	return func() { evidencePushInterval = old }
}

// SetPrimaryIPFunc substitutes the default-route source-IP resolver and returns
// the function that restores it.
func SetPrimaryIPFunc(f func() (string, error)) (restore func()) {
	old := primaryIPFunc
	primaryIPFunc = f
	return func() { primaryIPFunc = old }
}

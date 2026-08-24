package config

// Effective is the configuration the engine actually runs on.
//
// It is the parsed file everywhere except under hover, where one key changes:
// probe_unknown is forced on. Hover derives every threshold from how far
// through its own window each account is, and a window that has never been
// spent against reports no reset -- so it has no share elapsed, falls back to a
// fixed 80, and stays there. Only a turn spent against that window puts a reset
// on it, so probe_unknown off under hover is the one state hover cannot climb
// out of by itself.
//
// It is deliberately NOT applied by Parse or by Document.Config. `ccdad config
// list` reads those, and a listing that printed `probe_unknown true` beside the
// word `file` for a file that says `false` would be a falsehood in the column
// whose whole job is provenance. The listing marks the key overridden instead,
// and this is where the override actually happens.
func (c Config) Effective() Config {
	if !c.Hover {
		return c
	}
	c.ProbeUnknown = true
	return c
}

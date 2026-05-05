package config

import "time"

// Duration is a time.Duration that decodes from TOML strings via
// time.ParseDuration ("1s", "5m", "500ms", …). Zero value means
// "unset"; the loader fills in defaults afterwards.
type Duration time.Duration

func (d Duration) Std() time.Duration { return time.Duration(d) }

func (d *Duration) UnmarshalText(text []byte) error {
	if len(text) == 0 {
		*d = 0
		return nil
	}
	v, err := time.ParseDuration(string(text))
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

func (d Duration) MarshalText() ([]byte, error) {
	return []byte(time.Duration(d).String()), nil
}

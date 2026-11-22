package spokebook

import "testing"

func TestEnvBool(t *testing.T) {
	const key = "APP_NOTIFY_ALLOW_PRIVATE"
	for _, tc := range []struct {
		v       string
		want    bool
		wantErr bool
	}{
		{"", false, false},
		{"true", true, false},
		{"1", true, false},
		{"false", false, false},
		{"0", false, false},
		{"yes", false, true},
	} {
		t.Setenv(key, tc.v)
		got, err := envBool(key)
		if got != tc.want || (err != nil) != tc.wantErr {
			t.Errorf("%q: %v, %v", tc.v, got, err)
		}
	}
}

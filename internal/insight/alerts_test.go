package insight

import (
	"testing"
	"time"
)

func TestShortAge(t *testing.T) {
	for d, want := range map[time.Duration]string{
		30 * time.Second:                 "30s",
		45 * time.Minute:                 "45m",
		2 * time.Hour:                    "2h",
		2*time.Hour + 40*time.Minute + 5: "2h40m",
		26 * time.Hour:                   "26h",
	} {
		if got := shortAge(d); got != want {
			t.Fatalf("shortAge(%s)=%s want %s", d, got, want)
		}
	}
}

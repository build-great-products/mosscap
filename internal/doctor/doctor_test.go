package doctor

import "testing"

func TestOverall(t *testing.T) {
	tests := []struct {
		name   string
		checks []Check
		want   CheckStatus
	}{
		{name: "empty", want: Skipped},
		{name: "skipped", checks: []Check{{Status: Skipped}}, want: Skipped},
		{name: "pass", checks: []Check{{Status: Pass}}, want: Pass},
		{name: "pass and skipped", checks: []Check{{Status: Pass}, {Status: Skipped}}, want: Pass},
		{name: "warn takes precedence over pass", checks: []Check{{Status: Pass}, {Status: Warn}}, want: Warn},
		{name: "fail takes precedence over warn", checks: []Check{{Status: Warn}, {Status: Fail}}, want: Fail},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Overall(tt.checks); got != tt.want {
				t.Fatalf("Overall() = %s, want %s", got, tt.want)
			}
		})
	}
}

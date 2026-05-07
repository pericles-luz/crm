package aipanel

import "testing"

func TestBucketFromMs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		ms   int64
		want string
	}{
		{"zero floors to one", 0, "1"},
		{"negative floors to one", -2000, "1"},
		{"sub-second rounds up to one", 999, "1"},
		{"exactly one second", 1000, "1"},
		{"just over one second ceils to two", 1001, "2"},
		{"exactly two seconds", 2000, "2"},
		{"1.5s ceils to two", 1500, "2"},
		{"12500ms ceils to thirteen", 12500, "13"},
		{"thirty seconds", 30000, "30"},
		{"exactly sixty seconds", 60000, "60"},
		{"just over sixty overflows", 60001, "overflow"},
		{"large value overflows", 999999, "overflow"},
		{"max int64 overflows", 1<<62 - 1, "overflow"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := bucketFromMs(tc.ms); got != tc.want {
				t.Fatalf("bucketFromMs(%d) = %q, want %q", tc.ms, got, tc.want)
			}
		})
	}
}

package livecheck

import "testing"

func TestClassifyWebSession(t *testing.T) {
	cases := []struct {
		code int
		body string
		want string
	}{
		{200, `{"accessToken":"tok"}`, StatusAlive},
		{200, `{"user":{"email":"a@b.c"}}`, StatusDead},
		{200, `{}`, StatusDead},
		{401, `{}`, StatusDead},
		{403, `challenge`, StatusUnknown},
		{429, `rate`, StatusUnknown},
		{500, `err`, StatusUnknown},
		{200, `not-json`, StatusUnknown},
	}
	for _, c := range cases {
		if got := classifyWebSession(c.code, c.body); got != c.want {
			t.Errorf("classifyWebSession(%d, %q)=%s want %s", c.code, c.body, got, c.want)
		}
	}
}

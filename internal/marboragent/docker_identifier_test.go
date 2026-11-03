package marboragent

import "testing"

func TestValidateDockerContainer(t *testing.T) {
	cases := []struct {
		name string
		ok   bool
	}{
		{"ollama", true},
		{"my_vllm-1.2", true},
		{"0123456789ab", true},
		{"", false},
		{"-u", false},
		{"--privileged", false},
		{"a b", false},
		{"a;rm", false},
		{"a/b", false},
	}
	for _, c := range cases {
		if err := validateDockerContainer(c.name); (err == nil) != c.ok {
			t.Errorf("validateDockerContainer(%q) ok=%v, want %v", c.name, err == nil, c.ok)
		}
	}
}

package build

import (
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

func TestPort(t *testing.T) {
	image := func(ports []string, env []string, history ...string) *v1.ConfigFile {
		cfg := &v1.ConfigFile{Config: v1.Config{Env: env}}
		if ports != nil {
			cfg.Config.ExposedPorts = map[string]struct{}{}
			for _, p := range ports {
				cfg.Config.ExposedPorts[p] = struct{}{}
			}
		}
		for _, h := range history {
			cfg.History = append(cfg.History, v1.History{CreatedBy: h})
		}
		return cfg
	}
	for _, tc := range []struct {
		name string
		cfg  *v1.ConfigFile
		want int
	}{
		{"nothing exposed", image(nil, nil), DefaultPort},
		{"one port", image([]string{"8080/tcp"}, nil), 8080},
		{"a PORT variable", image(nil, []string{"PATH=/bin", "PORT=4000"}), 4000},
		{"EXPOSE beats the PORT variable", image([]string{"3000/tcp"}, []string{"PORT=4000"}), 3000},
		{"only udp exposed", image([]string{"53/udp"}, nil), DefaultPort},
		{"nginx base with BuildKit history", image([]string{"80/tcp", "3000/tcp"}, nil,
			`ADD alpine-minirootfs.tar.gz / # buildkit`,
			`EXPOSE map[80/tcp:{}]`,
			`CMD ["nginx" "-g" "daemon off;"]`,
			`RUN /bin/sh -c sed -i 's/listen 80/listen 3000/' /etc/nginx/conf.d/default.conf # buildkit`,
			`EXPOSE map[3000/tcp:{}]`), 3000},
		{"classic builder history", image([]string{"80/tcp", "3000/tcp"}, nil,
			`/bin/sh -c #(nop)  EXPOSE 80`,
			`/bin/sh -c #(nop)  EXPOSE 3000/tcp`), 3000},
		{"the app's EXPOSE comes before its CMD", image([]string{"80/tcp", "9000/tcp"}, nil,
			`EXPOSE map[80/tcp:{}]`,
			`EXPOSE map[9000/tcp:{}]`,
			`CMD ["./server"]`), 9000},
		{"a newer udp EXPOSE is skipped", image([]string{"80/tcp", "3000/tcp", "53/udp"}, nil,
			`EXPOSE map[80/tcp:{}]`,
			`EXPOSE map[3000/tcp:{}]`,
			`EXPOSE map[53/udp:{}]`), 3000},
		{"RUN mentioning EXPOSE is not an EXPOSE step", image([]string{"80/tcp", "3000/tcp"}, nil,
			`EXPOSE map[3000/tcp:{}]`,
			`RUN /bin/sh -c echo EXPOSE 80 # buildkit`), 3000},
		{"several in the newest step: the lowest", image([]string{"80/tcp", "8080/tcp", "9229/tcp"}, nil,
			`EXPOSE map[80/tcp:{}]`,
			`EXPOSE map[8080/tcp:{} 9229/tcp:{}]`), 8080},
		{"no history: the lowest", image([]string{"8080/tcp", "443/tcp"}, nil), 443},
	} {
		if got, from := Port(tc.cfg); got != tc.want {
			t.Errorf("%s: port %d (%s), want %d", tc.name, got, from, tc.want)
		}
	}
}

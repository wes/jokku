// Package guest is the init process (PID 1) inside every microVM, and the
// config it boots from.
//
// The jokku binary itself is copied into each rootfs as /.jokku/init, so
// there is no second binary to build or ship. The host hands the guest its
// settings on a small read-only config drive (/dev/vdb).
package guest

import "encoding/json"

// Device layout of every microVM, in the order the drives are attached.
const (
	RootDevice    = "/dev/vda" // the release's rootfs, read-only, shared by instances
	ConfigDevice  = "/dev/vdb" // Config as JSON, NUL padded
	ScratchDevice = "/dev/vdc" // per-instance ext4, the writable overlay layer

	// InitPath is where the init binary lives in every rootfs, and MountDir
	// is an empty directory init uses as a staging mount point.
	InitPath = "/.jokku/init"
	MountDir = "/.jokku/mnt"
)

// VolumeDevice is the device of the i-th volume, after the scratch disk.
func VolumeDevice(i int) string { return "/dev/vd" + string(rune('d'+i)) }

// Config is everything the guest needs to start the app.
type Config struct {
	Argv        []string `json:"argv"`
	Env         []string `json:"env"` // KEY=value, complete (image env, config vars, PORT)
	User        string   `json:"user,omitempty"`
	WorkDir     string   `json:"workdir,omitempty"`
	Hostname    string   `json:"hostname"`
	IP          string   `json:"ip"`
	DNS         []string `json:"dns,omitempty"`
	Search      []string `json:"search,omitempty"`      // DNS search domains
	StopTimeout int      `json:"stop_timeout"`          // seconds between the stop signal and SIGKILL
	StopSignal  string   `json:"stop_signal,omitempty"` // default SIGTERM; the image's STOPSIGNAL
	Mounts      []Mount  `json:"mounts,omitempty"`
	// AgentToken authorizes sessions (jokku enter, volume copies) with the
	// guest agent. Empty turns the agent off.
	AgentToken string `json:"agent_token,omitempty"`
}

// Mount is a volume: an ext4 disk mounted at Path before the app starts.
type Mount struct {
	Device string `json:"device"` // /dev/vdd
	Path   string `json:"path"`
}

// Encode renders the config drive: JSON padded with NULs to whole sectors,
// since block devices are sized in 512-byte sectors.
func (c Config) Encode() ([]byte, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	padded := make([]byte, (len(b)/512+1)*512)
	copy(padded, b)
	return padded, nil
}

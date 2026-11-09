module github.com/rake-pro/tasmonaut

go 1.27.3

// No Multipath TCP listeners: nothing on the fleet dials MPTCP, and a listening
// MPTCP socket was the precondition of the CVE-2026-98432 host-kernel exploit.
godebug multipathtcp=0

require (
	github.com/rs/zerolog v1.35.1
	go.yaml.in/yaml/v3 v3.0.6
)

require (
	github.com/mattn/go-colorable v0.1.16 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	golang.org/x/sys v0.49.0 // indirect
)

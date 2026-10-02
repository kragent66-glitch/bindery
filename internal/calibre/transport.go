package calibre

import "strings"

// Transport is which side opens the connection in plugin mode (#2833).
//
// Push is the original shape: Bindery posts a file path to the plugin's HTTP
// server, so Calibre must see the library on a shared drive and accept an
// inbound connection. Pull reverses it: the plugin connects out to Bindery's
// /bridge/v1 routes, downloads each pending delivery and acknowledges it, and
// the push worker stands down so a file is never sent both ways.
type Transport string

const (
	// TransportPush is Bindery sending books to the plugin. The default.
	TransportPush Transport = "push"
	// TransportPull is the plugin fetching books from Bindery.
	TransportPull Transport = "pull"
)

// ParseTransport reads a stored setting. Anything but "pull" is push, so an
// absent or mangled value keeps the behaviour every install had before.
func ParseTransport(s string) Transport {
	if strings.EqualFold(strings.TrimSpace(s), string(TransportPull)) {
		return TransportPull
	}
	return TransportPush
}

// Valid reports whether t is one of the canonical transports.
func (t Transport) Valid() bool {
	return t == TransportPush || t == TransportPull
}

func (t Transport) String() string { return string(t) }

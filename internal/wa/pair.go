package wa

import (
	"errors"
	"fmt"
	"io"
	"runtime"
	"time"

	"github.com/mdp/qrterminal/v3"
	"go.mau.fi/whatsmeow"
	wastore "go.mau.fi/whatsmeow/store"
	"google.golang.org/protobuf/proto"
)

// ConfigurePairing sets the device props sent while pairing: the device name
// shown on the phone, a required full history sync and generous history
// limits, so the first sync brings as much history as the phone allows.
func ConfigurePairing(deviceName string) {
	wastore.SetOSInfo(deviceName, [3]uint32{1, 0, 0})
	wastore.DeviceProps.RequireFullSync = proto.Bool(true)
	h := wastore.DeviceProps.HistorySyncConfig
	h.FullSyncDaysLimit = proto.Uint32(3650)
	h.FullSyncSizeMbLimit = proto.Uint32(102400)
	h.StorageQuotaMb = proto.Uint32(102400)
	h.RecentSyncDaysLimit = proto.Uint32(3650)
}

// pairClientName is how a phone-code pairing names this device while it
// links: WhatsApp expects "Browser (OS)".
var pairClientName = map[bool]string{true: "Chrome (Mac OS)", false: "Chrome (Windows)"}[runtime.GOOS == "darwin"]

// pairStall bounds how long pairing waits on history blobs that make no progress.
const pairStall = 10 * time.Minute

// PairOptions choose how Pair links the device.
type PairOptions struct {
	// Phone (international number, digits only) shows one pairing code to
	// enter on that phone instead of QR codes.
	Phone string
	// Rounds is how many QR logins Pair runs before giving up; each lasts
	// about 160 s, until the server stops issuing codes. Zero means one.
	// A pairing code always gets one round: a new code needs relaying.
	Rounds int
	// QR receives each QR code. Nil prints it to out as a terminal QR.
	QR func(code string)
	// Paired is told the linked account's phone number (digits) on success.
	Paired func(phone string)
}

// Pair links this profile as a new device, then stays connected while the
// initial history sync arrives. It returns once no notification arrived for
// idle and every archived notification and blob has been downloaded and
// ingested.
func (c *Client) Pair(out io.Writer, idle time.Duration, o PairOptions) error {
	if c.device.ID != nil {
		return fmt.Errorf("profile already paired as %s (to link it anew, run unlink first: it logs this device out)", c.device.ID.ToNonAD())
	}
	c.newWA()
	c.startWorker()
	rounds := max(o.Rounds, 1)
	if o.Phone != "" {
		rounds = 1
	}
	for round := 1; ; round++ {
		paired, err := c.pairRound(out, o)
		if err != nil {
			return err
		}
		if paired {
			break
		}
		if o.Phone != "" {
			return errors.New("pairing code not entered in time: run pair again for a new code")
		}
		if round >= rounds {
			return errors.New("QR code not scanned in time: run pair again")
		}
		fmt.Fprintln(out, "The QR codes ran out: asking WhatsApp for new ones.")
	}
	c.mu.Lock()
	c.st.AccountPN, c.st.AccountLID = c.device.GetJID().ToNonAD(), c.device.GetLID().ToNonAD()
	c.mu.Unlock()
	fmt.Fprintf(out, "Paired as %s. Receiving history; this ends %s after the last history notification.\n", c.device.GetJID().ToNonAD(), idle)
	if o.Paired != nil {
		o.Paired(c.device.GetJID().User)
	}
	if waitQuiet(c.ctx, c.arrivals, c.pendingHistory, idle, 5*time.Second, pairStall, out) {
		fmt.Fprintln(out, "History sync quiet and fully stored. Start the daemon with `whatsapp-mcp install`, or wait for its task if already installed.")
		return nil
	}
	n := c.pendingHistory()
	c.log.Warn("pairing ends with history blobs still failing; the daemon retries them", "pending", n)
	fmt.Fprintf(out, "History sync quiet; %d history blobs are not stored yet and the daemon retries them. Start it with `whatsapp-mcp install`, or wait for its task if already installed.\n", n)
	return nil
}

// pairRound runs one login: it connects, shows QR codes (or one pairing
// code) until the phone links or the server stops issuing codes, and
// reports whether the phone linked.
func (c *Client) pairRound(out io.Writer, o PairOptions) (bool, error) {
	// A round that ran out of codes closes its channel before whatsmeow
	// disconnects: wait for that disconnect, or it would cut the new login.
	for deadline := time.Now().Add(15 * time.Second); c.wa.IsConnected() && time.Now().Before(deadline); {
		time.Sleep(100 * time.Millisecond)
	}
	qr, err := c.wa.GetQRChannel(c.ctx)
	if err != nil {
		return false, err
	}
	if err := c.wa.Connect(); err != nil {
		return false, fmt.Errorf("connect: %w", err)
	}
	// The channel closes after success or timeout: read it to the end.
	paired, timedOut, codeShown := false, false, false
	for item := range qr {
		switch item.Event {
		case "code":
			if o.Phone == "" {
				if o.QR != nil {
					o.QR(item.Code)
					continue
				}
				fmt.Fprintln(out, "\nScan with WhatsApp > Settings > Linked devices > Link a device:")
				qrterminal.GenerateHalfBlock(item.Code, qrterminal.L, out)
				continue
			}
			// QR codes keep coming while the code waits: they are ignored, and
			// the login closes when they run out, about 160 s after connecting.
			if codeShown {
				continue
			}
			code, err := c.wa.PairPhone(c.ctx, o.Phone, true, whatsmeow.PairClientChrome, pairClientName)
			if err != nil {
				return false, fmt.Errorf("request a pairing code for +%s: %w", o.Phone, err)
			}
			codeShown = true
			fmt.Fprintf(out, "\nPairing code: %s\n"+
				"On the phone with +%s: WhatsApp > Settings > Linked devices > Link a device > Link with phone number instead, then enter the code.\n"+
				"WhatsApp may also show a notification to tap. The code expires in about 2 minutes.\n", code, o.Phone)
		case "success":
			paired = true
		case "timeout":
			timedOut = true
		default:
			if item.Error != nil {
				return false, fmt.Errorf("pairing failed (%s): %w", item.Event, item.Error)
			}
		}
	}
	switch {
	case paired:
		return true, nil
	case c.ctx.Err() != nil:
		return false, c.ctx.Err()
	case timedOut:
		return false, nil
	}
	return false, errors.New("pairing ended without success")
}

// ErrNotLinked means the profile has no linked device: never paired, or
// already removed from the phone.
var ErrNotLinked = errors.New("this profile is not linked to a phone")

// Unlink logs this linked device out: the phone drops it from Linked devices
// and whatsmeow deletes the session keys. Stored messages and media stay. A
// device the phone already removed is found out on connect, when whatsmeow
// deletes the session itself: that is ErrNotLinked too.
func (c *Client) Unlink(timeout time.Duration) error {
	if c.device.ID == nil {
		return ErrNotLinked
	}
	c.newWA()
	if err := c.wa.Connect(); err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	if !c.wa.WaitForConnection(timeout) {
		if c.device.ID == nil {
			return ErrNotLinked
		}
		return fmt.Errorf("not logged in to WhatsApp within %s", timeout)
	}
	return c.wa.Logout(c.ctx)
}

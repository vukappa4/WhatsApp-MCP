package wa

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waE2E"
	wastore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/vukappa4/whatsapp-mcp/internal/logx"
	"github.com/vukappa4/whatsapp-mcp/internal/people"
	"github.com/vukappa4/whatsapp-mcp/internal/store"
)

// State is the connection state shown by whatsapp_status and the freshness line.
type State string

const (
	StateNotPaired  State = "not_paired"
	StateConnecting State = "connecting"
	// StateReconnecting follows a system sleep or clock jump until WhatsApp
	// connects again: the old connection cannot be trusted.
	StateReconnecting State = "reconnecting"
	StateConnected    State = "connected"
	StateDisconnected State = "disconnected"
	StateLoggedOut    State = "logged_out"
	StateReplaced     State = "stream_replaced"
	StateBanned       State = "temporary_ban"
	StateOutdated     State = "client_outdated"
)

// OfflineUnknown means the offline queue may still be replaying: the client
// connected and WhatsApp has not yet said how much it holds or that it is done.
const OfflineUnknown = -1

// Status is a snapshot of the linked device.
type Status struct {
	State          State
	Since          time.Time // when State began
	Detail         string
	AccountPN      types.JID
	AccountLID     types.JID
	PushName       string
	OfflinePending int // missed events still replaying; 0 when caught up, OfflineUnknown while not known
	StartedAt      time.Time
	// StorageError is the last failed archive write, "" when storage works;
	// messages then go unacked and WhatsApp redelivers them.
	StorageError   string
	StorageErrorAt time.Time
	// BannedUntil is when WhatsApp's last temporary ban ends (zero: none);
	// sends are refused until then.
	BannedUntil time.Time
	BanReason   string
}

// Paired reports whether the profile has a session.
func (s Status) Paired() bool { return !s.AccountPN.IsEmpty() }

// Client owns the session DB, the whatsmeow client and the event handlers.
type Client struct {
	db        *store.DB
	log       *slog.Logger
	sessionDB *sql.DB
	container *sqlstore.Container
	device    *wastore.Device
	wa        *whatsmeow.Client
	ing       *Ingestor
	media     MediaSink       // optional: eager downloads and media retry answers
	ctx       context.Context // background work; cancelled by Stop after disconnecting
	cancel    context.CancelFunc
	dbCtx     context.Context // event handlers; never cancelled, so in-flight writes finish
	wg        sync.WaitGroup  // background goroutines
	handlers  sync.WaitGroup  // event handlers in flight

	sendPeer sendPeerFunc // to the account's primary phone; set with the whatsmeow client
	// decryptVote decrypts a poll vote; set with the whatsmeow client.
	decryptVote func(context.Context, *events.Message) *waE2E.PollVoteMessage
	groups      groupAPI   // group and newsletter metadata; set with the whatsmeow client
	history     historyAPI // history blob downloads; set with the whatsmeow client
	msgr        messenger  // sends and uploads; set with the whatsmeow client (guarded by mu)
	// syncIdentities applies the session's identities (SyncSessionIdentities).
	syncIdentities func(context.Context) error

	mu         sync.RWMutex
	st         Status
	syncing    bool
	syncAgain  bool // a trigger arrived during the identity sync
	naming     bool
	stopping   bool
	recovering map[appstate.WAPatchName]bool // app state recovery requests in flight
	refetches  map[appstate.WAPatchName]*refetchState
	// fetchAppState runs an app state sync; set with the whatsmeow client.
	fetchAppState func(ctx context.Context, name appstate.WAPatchName, fullSync, onlyIfNotSynced bool) error
	wake          chan struct{} // history worker: new notification archived
	arrivals      chan struct{} // pairing: a history notification arrived
	reconnect     func()        // renews the connection; set with the whatsmeow client
	// redelivery after a storage failure (storage.go)
	redeliverQueued bool
	redeliverWait   time.Duration
}

// OpenSession opens (and upgrades) a whatsmeow session DB.
func OpenSession(ctx context.Context, path string, log *slog.Logger) (*sql.DB, *sqlstore.Container, error) {
	sdb, err := sql.Open("sqlite", store.DSN(path))
	if err != nil {
		return nil, nil, err
	}
	container := sqlstore.NewWithDB(sdb, "sqlite", logx.WA(log, "session"))
	if err := container.Upgrade(ctx); err != nil {
		sdb.Close()
		return nil, nil, fmt.Errorf("upgrade session db %s: %w", path, err)
	}
	return sdb, container, nil
}

// NewClient opens the session and prepares the whatsmeow client without connecting.
func NewClient(ctx context.Context, sessionPath string, db *store.DB, log *slog.Logger) (*Client, error) {
	sdb, container, err := OpenSession(ctx, sessionPath, log)
	if err != nil {
		return nil, err
	}
	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		sdb.Close()
		return nil, fmt.Errorf("load device: %w", err)
	}
	c := &Client{db: db, log: log, sessionDB: sdb, container: container, device: device,
		dbCtx: context.WithoutCancel(ctx), wake: make(chan struct{}, 1), arrivals: make(chan struct{}, 64),
		recovering: map[appstate.WAPatchName]bool{}, refetches: map[appstate.WAPatchName]*refetchState{}}
	c.ctx, c.cancel = context.WithCancel(ctx)
	c.st = Status{State: StateNotPaired, Since: time.Now(), StartedAt: time.Now(), OfflinePending: OfflineUnknown}
	if device.ID != nil {
		c.st.State = StateConnecting
		c.st.AccountPN, c.st.AccountLID, c.st.PushName = device.ID.ToNonAD(), device.LID.ToNonAD(), device.PushName
	}
	c.loadBan(ctx, time.Now())
	c.ing = &Ingestor{DB: db, LIDs: device.LIDs, Log: log, Storage: c, Self: func() (types.JID, types.JID) {
		return device.GetJID().ToNonAD(), device.GetLID().ToNonAD()
	}}
	c.syncIdentities = func(ctx context.Context) error { return SyncSessionIdentities(ctx, c.ing, c.sessionDB) }
	return c, nil
}

// Ingestor exposes the client's ingestor (replay at startup, reingest).
func (c *Client) Ingestor() *Ingestor { return c.ing }

// SessionDB is the open whatsmeow session database.
func (c *Client) SessionDB() *sql.DB { return c.sessionDB }

// Status returns the current connection snapshot.
func (c *Client) Status() Status {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.st
}

func (c *Client) setState(s State, detail string) {
	c.mu.Lock()
	c.st.State, c.st.Since, c.st.Detail = s, time.Now(), detail
	c.mu.Unlock()
}

// logEvent appends to connection_log; errors are logged, never fatal.
func (c *Client) logEvent(event, detail string) {
	err := store.Retry(c.dbCtx, func() error { return store.LogConnection(c.dbCtx, c.db, time.Now(), event, detail) })
	if err != nil {
		c.log.Error("write connection_log", "event", event, "err", err)
	}
}

// Start replays archived events that were never ingested, then connects
// when the profile is paired. Unpaired profiles stay offline and only serve
// stored data.
func (c *Client) Start() error {
	if n, err := ExpirePending(c.dbCtx, c.db, time.Now()); err != nil {
		c.log.Warn("expire pending mutations", "err", err)
	} else if n > 0 {
		c.log.Info("dropped edits, revokes, reactions and votes whose message never arrived", "count", n, "after", PendingTTL)
	}
	if n, err := c.ing.ReplayPending(c.dbCtx); err != nil {
		c.log.Error("replay pending raw events", "err", err)
	} else if n > 0 {
		c.log.Info("replayed pending raw events", "count", n)
	}
	if c.device.ID == nil {
		c.logEvent(store.EvNotPaired, "")
		c.log.Warn("profile is not paired: serving stored data only")
		return nil
	}
	c.newWA()
	c.startWorker()
	c.wg.Add(2)
	go func() {
		defer c.wg.Done()
		c.connectLoop()
	}()
	go func() {
		defer c.wg.Done()
		c.watchClock()
	}()
	return nil
}

// configure sets the whatsmeow options the daemon relies on.
func configure(cli *whatsmeow.Client) {
	// Ack only after the handler returned, and keep decrypted plaintext in the
	// session DB until then: a handler that fails gets the same plaintext on
	// redelivery instead of a ratchet that already moved on.
	cli.SynchronousAck = true
	cli.EnableDecryptedEventBuffer = true
	// Download history blobs ourselves, after archiving the notification.
	cli.ManualHistorySyncDownload = true
	cli.EnableAutoReconnect = true
	cli.AutomaticMessageRerequestFromPhone = true
	// Full app state syncs and recoveries dispatch their archive, pin, mute
	// and mark-read mutations as events too, so chats get the current state
	// instead of only what changes afterwards.
	cli.EmitAppStateEventsOnFullSync = true
	// Never call SendPresence or MarkRead: the phone keeps notifications and unread badges.
}

func (c *Client) newWA() {
	c.wa = whatsmeow.NewClient(c.device, logx.WA(c.log, "wa"))
	configure(c.wa)
	c.wa.AddEventHandlerWithSuccessStatus(c.handle)
	cli := c.wa
	c.sendPeer = func(ctx context.Context, msg *waE2E.Message) error {
		_, err := cli.SendPeerMessage(ctx, msg)
		return err
	}
	c.groups = cli
	c.history = cli
	c.fetchAppState = cli.FetchAppState
	c.decryptVote = c.decryptPollVote
	c.mu.Lock()
	c.reconnect = cli.ResetConnection
	c.msgr = cli
	c.mu.Unlock()
}

// connectLoop retries the first connection with backoff; afterwards
// whatsmeow's auto-reconnect takes over.
func (c *Client) connectLoop() {
	backoff := 5 * time.Second
	for {
		err := c.wa.Connect()
		if err == nil || errors.Is(err, whatsmeow.ErrAlreadyConnected) {
			return
		}
		c.log.Warn("connect failed", "err", err, "retry_in", backoff)
		c.setState(StateDisconnected, err.Error())
		select {
		case <-c.ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 5*time.Minute)
	}
}

// Stop disconnects first (no new events), lets in-flight handlers finish,
// then stops background work and closes the session DB.
func (c *Client) Stop() {
	c.mu.Lock()
	c.stopping = true
	c.mu.Unlock()
	if c.wa != nil {
		c.wa.Disconnect()
	}
	done := make(chan struct{})
	go func() { c.handlers.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		c.log.Warn("event handlers still running at shutdown")
	}
	c.cancel()
	c.wg.Wait()
	c.sessionDB.Close()
}

func (c *Client) isStopping() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.stopping
}

// handle processes every whatsmeow event. Returning false for a message
// withholds the ack: with the decrypted event buffer WhatsApp redelivers the
// same plaintext. A message is acked as soon as it is archived in raw_events.
// A panic is logged and, for a message, withholds the ack (whatsmeow's own
// recover would ack it, and a message not archived would be lost).
func (c *Client) handle(evt any) (ok bool) {
	c.handlers.Add(1)
	defer c.handlers.Done()
	defer func() {
		if r := recover(); r != nil {
			c.log.Error("event handler panicked", "event", fmt.Sprintf("%T", evt), "panic", r, "stack", string(debug.Stack()))
			_, isMsg := evt.(*events.Message)
			ok = !isMsg
		}
	}()
	ctx := c.dbCtx
	switch v := evt.(type) {
	case *events.Message:
		return c.handleMessage(ctx, v)
	case *events.MediaRetry:
		if c.media != nil {
			c.media.MediaRetry(v)
		}

	case *events.Connected:
		c.mu.Lock()
		c.st.OfflinePending = OfflineUnknown // the offline queue replays now
		c.mu.Unlock()
		c.setState(StateConnected, "")
		c.logEvent(store.EvConnected, "")
		c.goSyncIdentities()
		c.goRefreshGroups()
		c.kickWorker()
	case *events.Disconnected:
		if c.isStopping() {
			return true // the clean stop is logged as daemon_stop
		}
		if s := c.Status().State; s == StateConnected || s == StateConnecting || s == StateReconnecting {
			c.setState(StateDisconnected, "")
		}
		c.logEvent(store.EvDisconnected, "")
	case *events.KeepAliveTimeout:
		c.logEvent(store.EvKeepAliveLost, fmt.Sprintf("errors=%d last_success=%s", v.ErrorCount, v.LastSuccess.Format(time.RFC3339)))
	case *events.KeepAliveRestored:
		c.logEvent(store.EvKeepAliveBack, "")
	case *events.LoggedOut:
		detail := v.Reason.String()
		c.setState(StateLoggedOut, detail)
		c.logEvent(store.EvLoggedOut, detail)
		c.log.Error("logged out by WhatsApp: pair again", "reason", detail, "on_connect", v.OnConnect)
	case *events.StreamReplaced:
		c.setState(StateReplaced, "another client connected with this session")
		c.logEvent(store.EvStreamReplaced, "")
		c.log.Error("stream replaced: another process uses this session")
	case *events.TemporaryBan:
		c.recordBan(ctx, v, time.Now())
		c.setState(StateBanned, v.String())
		c.logEvent(store.EvTemporaryBan, v.String())
		c.log.Error("temporary ban", "detail", v.String())
	case *events.ClientOutdated:
		c.setState(StateOutdated, "update go.mau.fi/whatsmeow and rebuild")
		c.logEvent(store.EvClientOutdated, "")
		c.log.Error("client outdated: update whatsmeow")
	case *events.ConnectFailure:
		c.logEvent(store.EvConnectFailure, fmt.Sprintf("%s %s", v.Reason, v.Message))
	case *events.OfflineSyncPreview:
		c.mu.Lock()
		c.st.OfflinePending = v.Total
		c.mu.Unlock()
		detail := fmt.Sprintf("total=%d messages=%d notifications=%d receipts=%d app_data=%d", v.Total, v.Messages, v.Notifications, v.Receipts, v.AppDataChanges)
		c.logEvent(store.EvOfflinePreview, detail)
		_ = store.SetState(ctx, c.db, store.KeyOfflineSync, fmt.Sprintf("pending:%d", v.Total))
	case *events.OfflineSyncCompleted:
		c.mu.Lock()
		c.st.OfflinePending = 0
		c.mu.Unlock()
		c.logEvent(store.EvOfflineDone, fmt.Sprintf("count=%d", v.Count))
		_ = store.SetState(ctx, c.db, store.KeyOfflineSync, fmt.Sprintf("done:%d", v.Count))
	case *events.AppStateSyncComplete:
		detail := fmt.Sprintf("%s v%d", v.Name, v.Version)
		if v.Recovery {
			detail += " (recovery)"
		}
		c.logEvent(store.EvAppStateSynced, detail)
		c.appStateSynced(v.Name, v.Version, v.Recovery)
		c.goSyncIdentities()
	case *events.AppStateSyncError:
		c.logEvent(store.EvAppStateError, fmt.Sprintf("%s full=%v: %v", v.Name, v.FullSync, v.Error))
		switch {
		case !errors.Is(v.Error, appstate.ErrMismatchingLTHash):
		case v.FullSync:
			// The server snapshot itself is broken: only the phone can help.
			c.goRecoverAppState(v.Name)
		default:
			c.goRefetchAppState(v.Name, time.Now())
		}
	case *events.PairSuccess:
		c.logEvent(store.EvPaired, v.ID.String())
		// A paired row starts a session for RecordDaemonStart: beat at once,
		// so a crash right after pairing is closed at its heartbeat.
		if err := store.Heartbeat(ctx, c.db, time.Now()); err != nil {
			c.log.Warn("heartbeat", "err", err)
		}

	case *events.PushName:
		c.update("push name", func(tx *sql.Tx) error {
			at := time.Now()
			if v.Message != nil {
				at = v.Message.Timestamp
			}
			pid, err := c.ing.person(ctx, tx, v.JID, v.JIDAlt, at)
			if err != nil {
				return err
			}
			return people.SetNames(ctx, tx, pid, people.Names{PushName: v.NewPushName})
		})
	case *events.BusinessName:
		c.update("business name", func(tx *sql.Tx) error {
			return c.ing.setNames(ctx, tx, v.JID, people.Names{BusinessName: v.NewBusinessName})
		})
	case *events.Contact:
		c.update("contact", func(tx *sql.Tx) error {
			return c.ing.setNames(ctx, tx, v.JID, people.Names{FullName: v.Action.GetFullName(), FirstName: v.Action.GetFirstName()})
		})
	case *events.GroupInfo:
		if err := c.ing.LiveGroupInfo(ctx, v); err != nil {
			c.log.Error("store group change", "group", v.JID, "err", err)
			if isArchiveError(err) {
				// Lost: WhatsApp does not redeliver group notifications. The
				// name refresh brings the current name and description back.
				c.goRefreshGroups()
				return false
			}
		}
	case *events.JoinedGroup:
		c.update("joined group", func(tx *sql.Tx) error {
			id, err := store.EnsureChat(ctx, tx, v.JID.String(), "group")
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE chats SET description = ? WHERE id = ? AND ? != ''`, v.GroupInfo.Topic, id, v.GroupInfo.Topic); err != nil {
				return err
			}
			return store.SetChatName(ctx, tx, id, v.GroupInfo.Name)
		})
	case *events.Archive, *events.Pin, *events.Mute, *events.MarkChatAsRead, *events.Receipt:
		if r, ok := v.(*events.Receipt); ok && r.IsFromMe && r.Sender.Device == 0 {
			c.phoneSeen(ctx, r.Timestamp) // the phone read something
		}
		if cs, ok := chatStateOf(v); ok {
			if err := c.ing.LiveChatState(ctx, cs); err != nil {
				c.log.Error("store chat state", "kind", cs.Kind, "chat", cs.JID, "err", err)
			}
		}
	}
	return true
}

// handleMessage archives a live message before decoding anything that can
// fail or panic (poll vote decryption, ingestion), then ingests it. It
// returns whether the message is safely archived, so it may be acked.
func (c *Client) handleMessage(ctx context.Context, v *events.Message) (archived bool) {
	defer func() {
		if r := recover(); r != nil {
			c.log.Error("message handler panicked", "id", v.Info.ID, "archived", archived, "panic", r, "stack", string(debug.Stack()))
		}
	}()
	if n := v.Message.GetProtocolMessage().GetHistorySyncNotification(); n != nil && v.Info.IsFromMe {
		if err := c.archiveNotification(ctx, v.Info.ID, n); err != nil {
			c.log.Error("archive history sync notification", "id", v.Info.ID, "err", err)
			return false
		}
		c.phoneSeen(ctx, v.Info.Timestamp) // only the phone sends history
		return true
	}
	ev, err := c.ing.ArchiveLive(ctx, v)
	if err != nil {
		c.log.Error("archive message", "id", v.Info.ID, "chat", v.Info.Chat, "err", err)
		return false
	}
	archived = true
	if v.Info.IsFromMe && v.Info.Sender.Device == 0 {
		c.phoneSeen(ctx, v.Info.Timestamp) // written on the phone
	}
	if v.Message.GetPollUpdateMessage() != nil && c.decryptVote != nil {
		if vote := c.decryptVote(ctx, v); vote != nil {
			if err := c.ing.AddPollVote(ctx, &ev, v, vote); err != nil {
				c.log.Warn("archive decrypted poll vote", "id", v.Info.ID, "err", err)
			}
		}
	}
	if err := c.ing.IngestArchived(ctx, ev); err != nil {
		c.log.Error("store message", "id", v.Info.ID, "chat", v.Info.Chat, "err", err)
		return true
	}
	c.queueLiveMedia(ctx, v)
	return true
}

// decryptPollVote decrypts a poll vote with whatsmeow; nil when it cannot,
// a panic in the decryption included.
func (c *Client) decryptPollVote(ctx context.Context, v *events.Message) (vote *waE2E.PollVoteMessage) {
	defer func() {
		if r := recover(); r != nil {
			c.log.Error("poll vote decryption panicked", "id", v.Info.ID, "panic", r)
			vote = nil
		}
	}()
	vote, err := c.wa.DecryptPollVote(ctx, v)
	if err != nil {
		c.log.Warn("decrypt poll vote", "id", v.Info.ID, "err", err)
		return nil
	}
	return vote
}

func (c *Client) update(what string, fn func(tx *sql.Tx) error) {
	err := store.Retry(c.dbCtx, func() error { return c.db.Tx(c.dbCtx, fn) })
	if err != nil {
		c.log.Error("update "+what, "err", err)
	}
}

// goSyncIdentities copies whatsmeow's LID map and address book into the
// people model, at most one run at a time. A trigger during a run queues one
// more run after it (several triggers queue one), so names an app-state sync
// brings meanwhile land at once instead of at the next connect.
func (c *Client) goSyncIdentities() {
	c.mu.Lock()
	if c.syncing {
		c.syncAgain = true
		c.mu.Unlock()
		return
	}
	c.syncing = true
	c.mu.Unlock()
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		for {
			if err := c.syncIdentities(c.ctx); err != nil && c.ctx.Err() == nil {
				c.log.Error("sync identities from session", "err", err)
			}
			c.mu.Lock()
			again := c.syncAgain && c.ctx.Err() == nil
			c.syncAgain = false
			c.syncing = again
			c.mu.Unlock()
			if !again {
				return
			}
		}
	}()
}

// SyncSessionIdentities applies the identities of an open session DB.
func SyncSessionIdentities(ctx context.Context, in *Ingestor, sessionDB *sql.DB) error {
	ids, err := ReadSession(ctx, sessionDB)
	if err != nil {
		return err
	}
	return in.ApplySession(ctx, ids, in.now())
}

// goRefreshGroups stores the current names and descriptions of joined
// groups and subscribed newsletters, then looks up groups that still have no
// name (see nameRefresh). At most one run at a time.
func (c *Client) goRefreshGroups() {
	c.mu.Lock()
	if c.naming || c.groups == nil {
		c.mu.Unlock()
		return
	}
	c.naming = true
	c.mu.Unlock()
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		defer func() { c.mu.Lock(); c.naming = false; c.mu.Unlock() }()
		r := nameRefresh{db: c.db, api: c.groups, now: time.Now, pause: 2 * time.Second, log: c.log.Info}
		if err := r.run(c.ctx); err != nil && c.ctx.Err() == nil {
			c.log.Warn("refresh group and newsletter names", "err", err)
		}
	}()
}

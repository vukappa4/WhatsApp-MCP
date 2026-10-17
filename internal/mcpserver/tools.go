package mcpserver

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vukappa4/whatsapp-mcp/internal/people"
	"github.com/vukappa4/whatsapp-mcp/internal/store"
	"github.com/vukappa4/whatsapp-mcp/internal/wa"
)

const (
	toolStatus    = "whatsapp_status"
	toolListChats = "whatsapp_list_chats"
	toolReadChat  = "whatsapp_read_chat"

	toolSearchPeople   = "whatsapp_search_people"
	toolSearchMessages = "whatsapp_search_messages"
)

func ptr[T any](v T) *T { return &v }

// relinkSteps tells Claude how to link this profile again. Pairing needs the
// profile, which this daemon holds, so the steps run in a shell with the
// installed program (this process): stop the daemon, pair, install again.
// The WhatsApp tools are gone meanwhile.
func (s *Server) relinkSteps() string {
	exe, err := os.Executable()
	if err != nil {
		exe = "whatsapp-mcp"
	}
	run := `"` + exe + `"`
	if runtime.GOOS == "windows" {
		run = "& " + run // PowerShell runs a quoted path only through &
	}
	p := s.Profile
	return fmt.Sprintf("To link again, run in a shell (these WhatsApp tools are unavailable until step 3 ends): "+
		"1. `%[1]s stop --profile %[2]s`. "+
		"2. At once, in the background: `%[1]s pair --profile %[2]s`. It opens a QR code page in the browser: the user scans it with the same WhatsApp number as before "+
		"(WhatsApp > Settings or the three-dot menu > Linked devices > Link a device). If it says the profile is already paired, run `%[1]s unlink --profile %[2]s` first, then pair. "+
		"If it says another process holds the profile, the autostart restarted the daemon: start again from step 1. "+
		"3. When pair has exited by itself (it prints \"History sync quiet\"), `%[1]s install --profile %[2]s`.", run, p)
}

func readOnly(title string) *mcp.ToolAnnotations {
	f := false
	return &mcp.ToolAnnotations{Title: title, ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: &f}
}

// schemaFor infers the input schema and adds enums the struct tags cannot express.
func schemaFor[T any](enums map[string][]any) *jsonschema.Schema {
	s, err := jsonschema.For[T](nil)
	if err != nil {
		panic(err)
	}
	for prop, vals := range enums {
		s.Properties[prop].Enum = vals
	}
	return s
}

var formatEnum = []any{"concise", "detailed"}

// textHandler answers a tool call with content only: text, plus images in
// whatsapp_get_media.
type textHandler[In any] func(context.Context, *mcp.CallToolRequest, In) (*mcp.CallToolResult, error)

// addTool registers a tool without an output schema and without
// structuredContent. A client may show the model only structuredContent
// when a tool has it, which would hide the transcript, the people and the
// ids: the text is the whole answer. With Out as any and no OutputSchema
// the SDK declares no schema, and a nil output leaves structuredContent out.
func addTool[In any](srv *mcp.Server, t *mcp.Tool, h textHandler[In]) {
	mcp.AddTool(srv, t, func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		res, err := h(ctx, req, in)
		return res, nil, err
	})
}

func (s *Server) addTools(srv *mcp.Server) {
	addTool(srv, &mcp.Tool{
		Name: toolStatus,
		Description: "Health of the WhatsApp link: connection, account, newest stored message, offline gaps, " +
			"history coverage, storage errors, a temporary ban and when the phone was last active (WhatsApp unlinks " +
			"this device after 14 days without it). Call it when results look stale or before trusting " +
			"that nothing new arrived.",
		Annotations: readOnly("WhatsApp status"),
	}, s.status)

	addTool(srv, &mcp.Tool{
		Name: toolListChats,
		Description: "List chats, newest message first, with the counterpart's name and phone, unread count and " +
			"a preview of the last message.",
		InputSchema: schemaFor[listChatsIn](map[string][]any{
			"filter":          {"all", "unread", "dm", "group"},
			"response_format": formatEnum,
		}),
		Annotations: readOnly("List WhatsApp chats"),
	}, s.listChats)

	addTool(srv, &mcp.Tool{
		Name: toolReadChat,
		Description: "Read the transcript of one chat, oldest to newest; without after, before or around, the latest " +
			"messages. Detailed mode cuts each text at 4,000 characters unless limit=1, and a long page leaves its " +
			"oldest messages to the cursor. Media, replies, edits, deletions and reactions are marked inline.",
		InputSchema: schemaFor[readChatIn](map[string][]any{"response_format": formatEnum}),
		Annotations: readOnly("Read a WhatsApp chat"),
	}, s.readChat)

	addTool(srv, &mcp.Tool{
		Name: toolSearchPeople,
		Description: "Find people, best match first: person id, name and where it comes from (address book or their " +
			"WhatsApp profile), phone numbers, their DM, the groups they wrote in and the last interaction. " +
			"Use it to pick the right person before reading or searching.",
		InputSchema: schemaFor[searchPeopleIn](map[string][]any{"response_format": formatEnum}),
		Annotations: readOnly("Search WhatsApp people"),
	}, s.searchPeople)

	addTool(srv, &mcp.Tool{
		Name: toolSearchMessages,
		Description: "Full-text search across all chats; without a query it lists the messages the filters select, " +
			"newest first. Open a hit with whatsapp_read_chat around=m_N.",
		InputSchema: schemaFor[searchMessagesIn](map[string][]any{
			"has_media":       {"image", "video", "voice", "audio", "document", "sticker", "any"},
			"sort":            {"relevance", "newest"},
			"response_format": formatEnum,
		}),
		Annotations: readOnly("Search WhatsApp messages"),
	}, s.searchMessages)

	addTool(srv, &mcp.Tool{
		Name: toolGetMedia,
		Description: "Open the media of one message: downloads it if needed and returns its local path and details. " +
			"Voice notes and audio come with their transcript; JPEG, PNG, GIF and WebP images under 1 MB (checked by " +
			"their bytes) also come as a picture; documents and videos as a path to open with other tools. If the " +
			"download or transcription takes longer, the result says when to call again.",
		InputSchema: schemaFor[getMediaIn](map[string][]any{
			"mode":            {"auto", "transcript", "path"},
			"response_format": formatEnum,
		}),
		// It downloads from WhatsApp and may ask the phone to upload the file
		// again: open-world, and a repeated call can give a different result.
		Annotations: &mcp.ToolAnnotations{Title: "Get WhatsApp media", ReadOnlyHint: true, OpenWorldHint: ptr(true)},
	}, s.getMedia)

	s.addSendTools(srv)
}

func textResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

// errResult is a tool-level error the model can act on.
func errResult(format string, args ...any) (*mcp.CallToolResult, error) {
	res := textResult(fmt.Sprintf(format, args...))
	res.IsError = true
	return res, nil
}

func detailed(f string) bool { return f == "detailed" }

// ---- whatsapp_status ----

type statusIn struct{}

func (s *Server) status(ctx context.Context, _ *mcp.CallToolRequest, _ statusIn) (*mcp.CallToolResult, error) {
	st := s.WA.Status()
	d, err := s.loadFresh(ctx)
	if err != nil {
		return nil, err
	}
	fresh := s.freshLine(d, s.now().Add(-7*24*time.Hour).Unix(), 0)
	stats, err := store.GetStats(ctx, s.DB)
	if err != nil {
		return nil, err
	}
	gaps := d.Gaps
	events, err := store.RecentConnections(ctx, s.DB, 8)
	if err != nil {
		return nil, err
	}
	histAt := d.HistorySyncAt
	histState, _, _ := store.GetState(ctx, s.DB, store.KeyHistorySyncState)
	imported, _, _ := store.GetState(ctx, s.DB, store.KeyImportLegacy)

	var b strings.Builder
	b.WriteString(fresh + "\n")
	fmt.Fprintf(&b, "Profile: %s (data dir %s)\n", s.Profile, s.DataDir)
	if st.Paired() {
		fmt.Fprintf(&b, "Account: %s, LID %s\n", people.FormatPhone("+"+st.AccountPN.User), st.AccountLID)
	} else {
		fmt.Fprintf(&b, "Account: not paired. %s\n", s.relinkSteps())
	}
	if st.State == wa.StateLoggedOut && st.Paired() {
		fmt.Fprintf(&b, "Logged out by WhatsApp. %s\n", s.relinkSteps())
	}
	fmt.Fprintf(&b, "Connection: %s since %s", st.State, fmtTS(st.Since.Unix()))
	if st.Detail != "" {
		fmt.Fprintf(&b, " (%s)", st.Detail)
	}
	if st.OfflinePending > 0 {
		fmt.Fprintf(&b, ", replaying %d missed events", st.OfflinePending)
	}
	if st.BannedUntil.After(s.now()) {
		fmt.Fprintf(&b, "\nTemporary ban: until %s (%s) - sends are refused until then", fmtTS(st.BannedUntil.Unix()), st.BanReason)
	}
	if st.StorageError != "" {
		fmt.Fprintf(&b, "\nStorage: ERROR since %s - new messages are not acked or saved; WhatsApp redelivers them once writes work (%s)",
			fmtTS(st.StorageErrorAt.Unix()), clip(st.StorageError, 200))
	}
	fmt.Fprintf(&b, "\nDaemon running since: %s\n", fmtTS(st.StartedAt.Unix()))
	fmt.Fprintf(&b, "Newest stored message: %s\n", orNever(fmtTS(stats.NewestTS)))

	b.WriteString("Offline gaps (daemon not connected):")
	switch {
	case len(gaps) == 0:
		b.WriteString(" none recorded\n")
	case len(gaps) > maxStatusGaps:
		fmt.Fprintf(&b, " %d recorded, the newest %d:\n", len(gaps), maxStatusGaps)
	default:
		fmt.Fprintf(&b, " %d recorded:\n", len(gaps))
	}
	for _, g := range gaps[max(0, len(gaps)-maxStatusGaps):] {
		to := "still open"
		if !g.To.IsZero() {
			to = fmtTS(g.To.Unix())
		}
		fmt.Fprintf(&b, "- %s -> %s (%s)\n", fmtTS(g.From.Unix()), to, g.Reason)
	}

	fmt.Fprintf(&b, "History: %d messages in %d chats, %d persons, %d media files", stats.Messages, stats.Chats, stats.Persons, stats.Media)
	if stats.OldestTS != 0 {
		fmt.Fprintf(&b, ", from %s to %s", fmtTS(stats.OldestTS), fmtTS(stats.NewestTS))
	}
	fmt.Fprintf(&b, "; %d chats have no human name yet\n", stats.UnnamedChats)
	if histAt.IsZero() {
		b.WriteString("History sync: none received yet\n")
	} else {
		fmt.Fprintf(&b, "History sync: last blob %s (%s)\n", fmtTS(histAt.Unix()), histState)
	}
	if imported != "" {
		fmt.Fprintf(&b, "Legacy import: %s\n", imported)
	}
	if s.Media != nil {
		ms, err := s.Media.Status(ctx)
		if err != nil {
			return nil, err
		}
		var counts []string
		for _, st := range store.MediaStates {
			counts = append(counts, fmt.Sprintf("%d %s", ms.Counts[st], st))
		}
		fmt.Fprintf(&b, "Media files: %s; download queue %d\n", strings.Join(counts, ", "), ms.DownloadQueue)
		if ms.Ready {
			engine := ms.Engine
			if ms.FFmpeg != "" {
				engine += ", ffmpeg " + ms.FFmpeg
			}
			fmt.Fprintf(&b, "Transcription: ready (%s); queue %d\n", engine, ms.TranscriptQueue)
		} else {
			fmt.Fprintf(&b, "Transcription: NOT READY - %s\n", ms.Detail)
		}
	}
	b.WriteString(phoneHealth(d.phoneSeen(), s.now()) + "\n")
	if len(events) > 0 {
		b.WriteString("Recent connection events:\n")
		for _, e := range events {
			line := fmt.Sprintf("%s %s", fmtTS(e.TS.Unix()), e.Event)
			if e.Detail != "" {
				line += " (" + clip(e.Detail, 120) + ")"
			}
			b.WriteString("- " + line + "\n")
		}
	}
	return textResult(strings.TrimRight(b.String(), "\n")), nil
}

// maxStatusGaps is how many of the newest offline gaps whatsapp_status lists.
const maxStatusGaps = 10

// phoneWarnAfter is when the status warns that the phone has been offline
// long: WhatsApp unlinks linked devices after 14 days without it.
const phoneWarnAfter = 12 * 24 * time.Hour

// phoneHealth reports the phone's last activity and warns before WhatsApp
// unlinks this device.
func phoneHealth(seen, now time.Time) string {
	if seen.IsZero() {
		return "Phone: no activity from the phone seen yet (a message written or read on it, a history sync). " +
			"WhatsApp unlinks this device after 14 days without the phone online."
	}
	days := int(now.Sub(seen).Hours() / 24)
	if now.Sub(seen) >= phoneWarnAfter {
		return fmt.Sprintf("Phone: WARNING - last active %s, %d days ago. WhatsApp unlinks this device after 14 days "+
			"without the phone online: open WhatsApp on the phone now.", fmtTS(seen.Unix()), days)
	}
	return fmt.Sprintf("Phone: last active %s (%d days ago); on-demand history and media re-upload need it online.", fmtTS(seen.Unix()), days)
}

func orNever(s string) string {
	if s == "" {
		return "never"
	}
	return s
}

// chatName renders the chat title: the person label for DMs, the wrapped
// group name otherwise.
func (w wrapper) chatName(c *store.ChatRow) string {
	if c.Kind == "dm" {
		if c.Person != nil && c.Person.IsMe {
			return "me (notes to self)"
		}
		return w.label(c.Person)
	}
	if c.Name == "" {
		return "unnamed " + c.Kind
	}
	return w.wrap(c.Name)
}

// ---- whatsapp_list_chats ----

type listChatsIn struct {
	Filter         string `json:"filter,omitempty" jsonschema:"default all"`
	Query          string `json:"query,omitempty" jsonschema:"substring of a chat name, person name or phone number"`
	Limit          int    `json:"limit,omitempty" jsonschema:"chats per page, 1-100, default 20"`
	Cursor         string `json:"cursor,omitempty" jsonschema:"next_cursor of the previous page; it carries filter and query"`
	ResponseFormat string `json:"response_format,omitempty" jsonschema:"default concise; detailed adds JIDs, archive and mute state"`
}

func (s *Server) listChats(ctx context.Context, _ *mcp.CallToolRequest, in listChatsIn) (*mcp.CallToolResult, error) {
	limit := in.Limit
	if limit == 0 {
		limit = 20
	}
	if limit < 1 || limit > 100 {
		return errResult("limit must be between 1 and 100, got %d", in.Limit)
	}
	f := store.ChatFilter{Limit: limit, Query: in.Query}
	switch in.Filter {
	case "", "all":
	case "unread":
		f.UnreadOnly = true
	case "dm", "group":
		f.Kind = in.Filter
	default:
		return errResult("filter must be all, unread, dm or group, got %q", in.Filter)
	}
	if in.Cursor != "" {
		c, err := decodeCursor(in.Cursor, toolListChats)
		if err != nil {
			return errResult("%v", err)
		}
		f.Kind, f.UnreadOnly, f.Query = c.Kind, c.Unread, c.Query
		f.HasCursor, f.AfterTS, f.AfterID = true, c.TS, c.ID
	}
	chats, more, err := store.ListChats(ctx, s.DB, f)
	if err != nil {
		return nil, err
	}
	fresh, err := s.freshness(ctx, s.now().Add(-7*24*time.Hour).Unix(), 0)
	if err != nil {
		return nil, err
	}
	w := newWrapper()
	full := detailed(in.ResponseFormat)

	var b strings.Builder
	b.WriteString(fresh + "\n")
	if len(chats) == 0 {
		b.WriteString("No chats match.")
		if f.Query != "" {
			b.WriteString(" Try a shorter query, a phone number, or no filter.")
		}
		return textResult(b.String()), nil
	}
	fmt.Fprintf(&b, "%d chats, newest first:\n", len(chats))
	for i := range chats {
		c := &chats[i]
		fmt.Fprintf(&b, "%s %s · %s · %s", chatID(c.ID), w.chatName(c), c.Kind, orNever(fmtTS(c.LastMessageAt)))
		if c.Unread > 0 {
			fmt.Fprintf(&b, " · %d unread", c.Unread)
		}
		if full {
			fmt.Fprintf(&b, " · jid %s", c.JID)
			if c.Person != nil {
				fmt.Fprintf(&b, " · person %s", personID(c.Person.ID))
			}
			if c.Archived {
				b.WriteString(" · archived")
			}
			if c.MutedUntil > s.now().Unix() {
				b.WriteString(" · muted")
			}
		}
		b.WriteString("\n")
		if m := c.Last; m != nil {
			n := 80
			if full {
				n = 300
			}
			who := w.sender(m)
			fmt.Fprintf(&b, "   last: %s: %s\n", who, w.summary(m, n))
		}
	}
	if more {
		last := chats[len(chats)-1]
		next := cursor{Tool: toolListChats, TS: last.LastMessageAt, ID: last.ID, Kind: f.Kind, Unread: f.UnreadOnly, Query: f.Query}.encode()
		fmt.Fprintf(&b, "More chats: call again with cursor=%q", next)
	}
	return textResult(strings.TrimRight(b.String(), "\n")), nil
}

// sender names the author of a message: me, the person, or the channel itself.
func (w wrapper) sender(m *store.MessageRow) string {
	switch {
	case m.FromMe:
		return "me"
	case m.Sender != nil:
		return w.name(m.Sender)
	}
	return "channel"
}

// summary is a one-line rendering of a message body for previews.
func (w wrapper) summary(m *store.MessageRow, n int) string {
	var parts []string
	if m.Kind != "text" {
		parts = append(parts, "["+m.Kind+"]")
	}
	if m.DeletedAt != 0 {
		parts = append(parts, "[deleted]")
	}
	if t := clip(m.Text, n); t != "" {
		parts = append(parts, w.wrap(t))
	}
	return strings.Join(parts, " ")
}

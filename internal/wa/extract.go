package wa

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"go.mau.fi/whatsmeow/proto/waE2E"
)

// unwrap peels every container WhatsApp puts around real content: device
// sent, ephemeral, view-once (all versions), documentWithCaption, edited,
// bot invoke, lottie sticker, group-mention and associated-child (album
// items) wrappers. Live events arrive already unwrapped once by whatsmeow;
// history sync messages do not, and wrappers can nest, so both paths run
// through here.
func unwrap(m *waE2E.Message) *waE2E.Message {
	m, _ = unwrapAssoc(m)
	return m
}

// unwrapAssoc unwraps like unwrap and also returns the first message
// association found on any layer (album item, HD copy...).
func unwrapAssoc(m *waE2E.Message) (*waE2E.Message, *waE2E.MessageAssociation) {
	var assoc *waE2E.MessageAssociation
	for range 16 {
		if m == nil {
			return nil, assoc
		}
		if assoc == nil {
			assoc = m.GetMessageContextInfo().GetMessageAssociation()
		}
		var next *waE2E.Message
		switch {
		case m.GetAssociatedChildMessage().GetMessage() != nil:
			next = m.GetAssociatedChildMessage().GetMessage()
		case m.GetDeviceSentMessage().GetMessage() != nil:
			next = m.GetDeviceSentMessage().GetMessage()
		case m.GetEphemeralMessage().GetMessage() != nil:
			next = m.GetEphemeralMessage().GetMessage()
		case m.GetViewOnceMessage().GetMessage() != nil:
			next = m.GetViewOnceMessage().GetMessage()
		case m.GetViewOnceMessageV2().GetMessage() != nil:
			next = m.GetViewOnceMessageV2().GetMessage()
		case m.GetViewOnceMessageV2Extension().GetMessage() != nil:
			next = m.GetViewOnceMessageV2Extension().GetMessage()
		case m.GetDocumentWithCaptionMessage().GetMessage() != nil:
			next = m.GetDocumentWithCaptionMessage().GetMessage()
		case m.GetEditedMessage().GetMessage() != nil:
			next = m.GetEditedMessage().GetMessage()
		case m.GetBotInvokeMessage().GetMessage() != nil:
			next = m.GetBotInvokeMessage().GetMessage()
		case m.GetLottieStickerMessage().GetMessage() != nil:
			next = m.GetLottieStickerMessage().GetMessage()
		case m.GetGroupMentionedMessage().GetMessage() != nil:
			next = m.GetGroupMentionedMessage().GetMessage()
		}
		if next == nil {
			return m, assoc
		}
		m = next
	}
	return m, assoc
}

// duplicateAssoc reports association types whose child is another copy of
// the parent's media (HD or HEVC upload, motion photo): the parent row
// already shows it, so the child is not stored.
func duplicateAssoc(a *waE2E.MessageAssociation) bool {
	switch a.GetAssociationType() {
	case waE2E.MessageAssociation_HD_IMAGE_DUAL_UPLOAD, waE2E.MessageAssociation_HD_VIDEO_DUAL_UPLOAD,
		waE2E.MessageAssociation_HEVC_VIDEO_DUAL_UPLOAD, waE2E.MessageAssociation_MOTION_PHOTO:
		return a.GetParentMessageKey().GetID() != ""
	}
	return false
}

// mediaInfo is everything needed to download the file later.
type mediaInfo struct {
	Mimetype      string
	Filename      string
	Size          uint64
	Duration      uint32
	DirectPath    string
	URL           string
	MediaKey      []byte
	FileSHA256    []byte
	FileEncSHA256 []byte
}

// content is the storable part of a message.
type content struct {
	Kind  string
	Text  string
	Media *mediaInfo
	Ctx   *waE2E.ContextInfo
	Album string // wa_id of the album message this item belongs to
}

type downloadable interface {
	GetMimetype() string
	GetFileLength() uint64
	GetDirectPath() string
	GetURL() string
	GetMediaKey() []byte
	GetFileSHA256() []byte
	GetFileEncSHA256() []byte
}

func media(d downloadable) *mediaInfo {
	return &mediaInfo{
		Mimetype:      d.GetMimetype(),
		Size:          d.GetFileLength(),
		DirectPath:    d.GetDirectPath(),
		URL:           d.GetURL(),
		MediaKey:      d.GetMediaKey(),
		FileSHA256:    d.GetFileSHA256(),
		FileEncSHA256: d.GetFileEncSHA256(),
	}
}

// extract returns the storable content of an unwrapped message. ok is false
// for messages with nothing a person would see (key distribution, protocol
// noise, encrypted poll votes...).
func extract(m *waE2E.Message) (c content, ok bool) {
	switch {
	case m == nil:
		return c, false
	case m.Conversation != nil:
		return content{Kind: "text", Text: m.GetConversation()}, m.GetConversation() != ""
	case m.ExtendedTextMessage != nil:
		x := m.GetExtendedTextMessage()
		return content{Kind: "text", Text: x.GetText(), Ctx: x.GetContextInfo()}, true
	case m.ImageMessage != nil:
		x := m.GetImageMessage()
		return content{Kind: "image", Text: x.GetCaption(), Media: media(x), Ctx: x.GetContextInfo()}, true
	case m.VideoMessage != nil || m.PtvMessage != nil:
		x := m.GetVideoMessage()
		if x == nil {
			x = m.GetPtvMessage()
		}
		md := media(x)
		md.Duration = x.GetSeconds()
		return content{Kind: "video", Text: x.GetCaption(), Media: md, Ctx: x.GetContextInfo()}, true
	case m.AudioMessage != nil:
		x := m.GetAudioMessage()
		md := media(x)
		md.Duration = x.GetSeconds()
		kind := "audio"
		if x.GetPTT() {
			kind = "voice"
		}
		return content{Kind: kind, Media: md, Ctx: x.GetContextInfo()}, true
	case m.DocumentMessage != nil:
		x := m.GetDocumentMessage()
		md := media(x)
		md.Filename = x.GetFileName()
		if md.Filename == "" {
			md.Filename = x.GetTitle()
		}
		return content{Kind: "document", Text: x.GetCaption(), Media: md, Ctx: x.GetContextInfo()}, true
	case m.StickerMessage != nil:
		x := m.GetStickerMessage()
		return content{Kind: "sticker", Media: media(x), Ctx: x.GetContextInfo()}, true
	case m.LocationMessage != nil:
		x := m.GetLocationMessage()
		return content{Kind: "location", Text: locationText(x.GetName(), x.GetAddress(), x.GetComment(), x.GetDegreesLatitude(), x.GetDegreesLongitude()), Ctx: x.GetContextInfo()}, true
	case m.LiveLocationMessage != nil:
		x := m.GetLiveLocationMessage()
		return content{Kind: "live_location", Text: locationText("", "", x.GetCaption(), x.GetDegreesLatitude(), x.GetDegreesLongitude()), Ctx: x.GetContextInfo()}, true
	case m.ContactMessage != nil:
		x := m.GetContactMessage()
		return content{Kind: "contact", Text: contactText(x.GetDisplayName(), x.GetVcard()), Ctx: x.GetContextInfo()}, true
	case m.ContactsArrayMessage != nil:
		x := m.GetContactsArrayMessage()
		var parts []string
		for _, ct := range x.GetContacts() {
			parts = append(parts, contactText(ct.GetDisplayName(), ct.GetVcard()))
		}
		return content{Kind: "contact", Text: strings.Join(parts, "\n"), Ctx: x.GetContextInfo()}, true
	case pollCreation(m) != nil:
		x := pollCreation(m)
		var b strings.Builder
		b.WriteString(x.GetName())
		for _, o := range x.GetOptions() {
			b.WriteString("\n- " + o.GetOptionName())
		}
		return content{Kind: "poll", Text: b.String(), Ctx: x.GetContextInfo()}, true
	case m.CallLogMesssage != nil:
		x := m.GetCallLogMesssage()
		kind := "voice"
		if x.GetIsVideo() {
			kind = "video"
		}
		text := fmt.Sprintf("%s call, %s", kind, strings.ToLower(strings.ReplaceAll(x.GetCallOutcome().String(), "_", " ")))
		if d := x.GetDurationSecs(); d > 0 {
			text += fmt.Sprintf(", %dm%02ds", d/60, d%60)
		}
		return content{Kind: "call", Text: text}, true
	case m.EventMessage != nil:
		x := m.GetEventMessage()
		text := "Event: " + x.GetName()
		if d := x.GetDescription(); d != "" {
			text += "\n" + d
		}
		return content{Kind: "text", Text: text, Ctx: x.GetContextInfo()}, true
	case m.GroupInviteMessage != nil:
		x := m.GetGroupInviteMessage()
		text := "Group invite: " + x.GetGroupName()
		if cap := x.GetCaption(); cap != "" {
			text += "\n" + cap
		}
		return content{Kind: "text", Text: text, Ctx: x.GetContextInfo()}, true
	case m.ButtonsMessage != nil:
		x := m.GetButtonsMessage()
		return content{Kind: "text", Text: x.GetContentText(), Ctx: x.GetContextInfo()}, x.GetContentText() != ""
	case m.TemplateMessage != nil:
		x := m.GetTemplateMessage().GetHydratedTemplate()
		return content{Kind: "text", Text: x.GetHydratedContentText()}, x.GetHydratedContentText() != ""
	case m.ListMessage != nil:
		x := m.GetListMessage()
		text := strings.TrimSpace(x.GetTitle() + "\n" + x.GetDescription())
		return content{Kind: "text", Text: text, Ctx: x.GetContextInfo()}, text != ""
	case m.ButtonsResponseMessage != nil:
		x := m.GetButtonsResponseMessage()
		return selected(x.GetSelectedDisplayText(), x.GetSelectedButtonID(), x.GetContextInfo())
	case m.TemplateButtonReplyMessage != nil:
		x := m.GetTemplateButtonReplyMessage()
		return selected(x.GetSelectedDisplayText(), x.GetSelectedID(), x.GetContextInfo())
	case m.ListResponseMessage != nil:
		x := m.GetListResponseMessage()
		return selected(x.GetTitle(), x.GetSingleSelectReply().GetSelectedRowID(), x.GetContextInfo())
	case m.InteractiveResponseMessage != nil:
		x := m.GetInteractiveResponseMessage()
		return selected(x.GetBody().GetText(), "", x.GetContextInfo())
	case m.InteractiveMessage != nil:
		x := m.GetInteractiveMessage()
		return content{Kind: "text", Text: x.GetBody().GetText(), Ctx: x.GetContextInfo()}, x.GetBody().GetText() != ""
	}
	return c, false
}

// selected renders a reply to buttons, lists or templates as the label the
// person tapped.
func selected(label, id string, ci *waE2E.ContextInfo) (content, bool) {
	if label == "" {
		label = id
	}
	if label == "" {
		return content{}, false
	}
	return content{Kind: "text", Text: "[selected] " + label, Ctx: ci}, true
}

func pollCreation(m *waE2E.Message) *waE2E.PollCreationMessage {
	for _, p := range []*waE2E.PollCreationMessage{
		m.GetPollCreationMessage(), m.GetPollCreationMessageV2(), m.GetPollCreationMessageV3(),
		m.GetPollCreationMessageV5(), m.GetPollCreationMessageV6(),
	} {
		if p != nil {
			return p
		}
	}
	return nil
}

func locationText(name, address, comment string, lat, lng float64) string {
	var parts []string
	for _, s := range []string{name, address, comment} {
		if s = strings.TrimSpace(s); s != "" {
			parts = append(parts, s)
		}
	}
	parts = append(parts, fmt.Sprintf("(%.6f, %.6f) https://maps.google.com/?q=%.6f,%.6f", lat, lng, lat, lng))
	return strings.Join(parts, ", ")
}

// contactText renders a shared contact as "Name: +39..., +39...".
func contactText(name, vcard string) string {
	var phones []string
	for _, line := range strings.Split(vcard, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(strings.ToUpper(line), "TEL") {
			continue
		}
		if i := strings.LastIndex(line, ":"); i >= 0 {
			if p := strings.TrimSpace(line[i+1:]); p != "" {
				phones = append(phones, p)
			}
		}
	}
	if len(phones) == 0 {
		return name
	}
	return name + ": " + strings.Join(phones, ", ")
}

// preview builds the short quoted-message preview stored with replies.
func preview(m *waE2E.Message) string {
	c, ok := extract(unwrap(m))
	if !ok {
		return ""
	}
	text := strings.Join(strings.Fields(c.Text), " ")
	if c.Kind != "text" {
		if text == "" {
			text = "[" + c.Kind + "]"
		} else {
			text = "[" + c.Kind + "] " + text
		}
	}
	return truncate(text, 120)
}

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

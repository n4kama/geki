// Geki — replaces :keyword: tags in chat messages with inline Discord emotes.
package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
)

const (
	serversPath = "servers.json"
	webhookName = "Geki"
	cmdPrefix   = "!geki" // !geki set <name> <https-url> / !geki allow <username>
)

type appEmoji struct {
	id       string
	animated bool
}

// serverData is the per-guild state persisted in servers.json.
type serverData struct {
	Whitelist []string            `json:"whitelist,omitempty"` // usernames allowed to run "!geki set"
	Overrides map[string]override `json:"overrides,omitempty"` // keyword -> this guild's emote
}

type override struct {
	URL      string `json:"url"`
	ID       string `json:"id"` // application-emoji ID, so we never re-upload
	Animated bool   `json:"animated"`
}

var (
	whMu     sync.Mutex
	webhooks = map[string][2]string{} // channelID -> {webhookID, token}

	appID     string
	emMu      sync.Mutex
	appEmojis = map[string]appEmoji{} // keyword -> global default application emoji

	srvMu   sync.Mutex
	servers = map[string]*serverData{} // guildID -> whitelist + per-guild overrides

	// A bare :alphanumeric: tag anywhere in a message is a candidate. The first
	// alternative matches a whole existing custom emoji (<:name:id> / <a:name:id>)
	// so the :name: inside a server's own emoji is consumed and left untouched.
	tagRe = regexp.MustCompile(`<a?:[a-zA-Z0-9_]+:[0-9]+>|:([a-zA-Z0-9]+):`)

	// The :keyword: a set command pins; alphanumeric, 2-64 chars. (The override's
	// actual Discord emoji name is hashed in overrideName, so the 32-char emoji
	// limit doesn't apply here.)
	forceNameRe = regexp.MustCompile(`^[a-zA-Z0-9]{2,64}$`)
)

// parseSet validates "!geki set <name> <url>", returning the lowercased name and
// url. ok is false for the wrong shape, a bad name, or a non-https url.
func parseSet(fields []string) (name, url string, ok bool) {
	if len(fields) != 4 || fields[0] != cmdPrefix || fields[1] != "set" {
		return "", "", false
	}
	name, url = strings.ToLower(fields[2]), fields[3]
	if !forceNameRe.MatchString(name) {
		return "", "", false
	}
	if !strings.HasPrefix(url, "https://") { // https only; internal IPs are blocked at dial time
		return "", "", false
	}
	return name, url, true
}

// replaceTags swaps each :tag: for whatever resolve returns; unresolved tags are
// left as-is. Reports whether anything was replaced.
func replaceTags(content string, resolve func(kw string) (string, bool)) (string, bool) {
	changed := false
	out := tagRe.ReplaceAllStringFunc(content, func(tok string) string {
		if tok[0] == '<' {
			return tok // already a custom emoji (e.g. the server's own) — leave it
		}
		kw := strings.ToLower(tok[1 : len(tok)-1])
		if rep, ok := resolve(kw); ok {
			changed = true
			return rep
		}
		return tok
	})
	return out, changed
}

type emoteFile struct {
	Name   string `json:"name"`
	Format string `json:"format"`
	Height int    `json:"height"`
}

type gqlResp struct {
	Data struct {
		Emotes struct {
			Items []struct {
				Animated bool `json:"animated"`
				Host     struct {
					URL   string      `json:"url"`
					Files []emoteFile `json:"files"`
				} `json:"host"`
			} `json:"items"`
		} `json:"emotes"`
	} `json:"data"`
}

// pickURL builds the CDN link: GIF for animated, PNG otherwise, at the 4x tier
// (height 128, the largest 7TV serves). uploadEmoji steps down if 4x is too big.
func pickURL(animated bool, host string, files []emoteFile) string {
	want := "PNG"
	if animated {
		want = "GIF"
	}
	for _, f := range files {
		if f.Format == want && f.Height == 128 {
			return "https:" + host + "/" + f.Name // host is "//cdn.7tv.app/emote/<id>"
		}
	}
	return ""
}

const gqlQuery = `query($q:String!,$f:EmoteSearchFilter){emotes(query:$q,limit:1,filter:$f){items{animated host{url files{name format height}}}}}`

// search7TV returns the most-popular exact-name match's emote URL, or "" if none.
// filter.category TOP ranks by Twitch channel count (same as the website);
// exact_match restricts to emotes named exactly the keyword.
func search7TV(kw string) string {
	body, _ := json.Marshal(map[string]any{
		"query": gqlQuery,
		"variables": map[string]any{
			"q": kw,
			"f": map[string]any{"category": "TOP", "exact_match": true, "case_sensitive": false},
		},
	})
	resp, err := http.Post("https://7tv.io/v3/gql", "application/json", bytes.NewReader(body))
	if err != nil {
		log.Printf("7tv %q: %v", kw, err)
		return ""
	}
	defer resp.Body.Close()

	var r gqlResp
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		log.Printf("7tv decode %q: %v", kw, err)
		return ""
	}
	if len(r.Data.Emotes.Items) == 0 {
		return ""
	}
	it := r.Data.Emotes.Items[0] // ranked by channel count via filter.category TOP
	return pickURL(it.Animated, it.Host.URL, it.Host.Files)
}

// emojiTag resolves a keyword to inline emoji markup. A per-guild override wins;
// otherwise the global default is used (uploaded + cached on first use).
func emojiTag(s *discordgo.Session, guildID, kw string) (string, bool) {
	if ov, ok := getOverride(guildID, kw); ok {
		return emojiMarkup(kw, ov.ID, ov.Animated), true
	}

	emMu.Lock()
	e, ok := appEmojis[kw]
	emMu.Unlock()
	if !ok {
		url := search7TV(kw)
		if url == "" {
			return "", false
		}
		var err error
		e, err = uploadEmoji(s, kw, url)
		if err != nil {
			log.Printf("upload emoji %q: %v", kw, err) // leave the raw tag in the text
			return "", false
		}
		emMu.Lock()
		appEmojis[kw] = e
		emMu.Unlock()
	}
	return emojiMarkup(kw, e.id, e.animated), true
}

// emojiMarkup builds inline emoji markup; name is cosmetic, Discord renders by id.
func emojiMarkup(name, id string, animated bool) string {
	if animated {
		return "<a:" + name + ":" + id + ">"
	}
	return "<:" + name + ":" + id + ">"
}

const maxEmojiBytes = 256 << 10 // Discord rejects emoji images larger than this

var tierRe = regexp.MustCompile(`/[1-4]x\.(png|gif|webp)$`)

// tierCandidates lists a 7TV emote URL at every size from largest to smallest,
// so we can upload the best one that fits. Non-7TV URLs pass through unchanged.
func tierCandidates(url string) []string {
	if !tierRe.MatchString(url) {
		return []string{url}
	}
	out := make([]string, 0, 4)
	for _, t := range []string{"4x", "3x", "2x", "1x"} {
		out = append(out, tierRe.ReplaceAllString(url, "/"+t+".$1"))
	}
	return out
}

// uploadEmoji registers the image as an application emoji, using the largest size
// tier that fits Discord's limit (PNGs upload at 4x; oversized GIFs step down).
func uploadEmoji(s *discordgo.Session, name, url string) (appEmoji, error) {
	for _, u := range tierCandidates(url) {
		data, err := fetchAtMost(u, maxEmojiBytes)
		if err != nil {
			return appEmoji{}, err
		}
		if data == nil {
			continue // too big, try a smaller tier
		}
		mime := "image/png"
		if strings.HasSuffix(u, ".gif") {
			mime = "image/gif"
		}
		uri := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
		e, err := s.ApplicationEmojiCreate(appID, &discordgo.EmojiParams{Name: name, Image: uri})
		if err != nil {
			return appEmoji{}, err
		}
		return appEmoji{id: e.ID, animated: e.Animated}, nil
	}
	return appEmoji{}, fmt.Errorf("no size of %s fits under %dKB", url, maxEmojiBytes>>10)
}

// safeClient blocks requests that resolve to non-public addresses. The Control
// hook runs for every dial (including redirects) against the actual IP, so DNS
// rebinding can't slip an internal target past us.
var safeClient = &http.Client{
	Timeout: 15 * time.Second,
	Transport: &http.Transport{
		DialContext: (&net.Dialer{
			Control: func(_, address string, _ syscall.RawConn) error {
				host, _, _ := net.SplitHostPort(address)
				if ip := net.ParseIP(host); ip == nil || !isPublicIP(ip) {
					return fmt.Errorf("blocked non-public address %s", address)
				}
				return nil
			},
		}).DialContext,
	},
}

func isPublicIP(ip net.IP) bool {
	return !(ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast())
}

// fetchAtMost returns the body if it is <= max bytes, or nil if it exceeds it.
func fetchAtMost(url string, max int) ([]byte, error) {
	resp, err := safeClient.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(max)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > max {
		return nil, nil
	}
	return data, nil
}

func loadServers() {
	b, err := os.ReadFile(serversPath)
	if err != nil {
		return // no file yet → no whitelists or overrides
	}
	if err := json.Unmarshal(b, &servers); err != nil {
		log.Fatalf("parse %s: %v", serversPath, err)
	}
}

// saveServers persists per-guild state. Caller holds srvMu.
func saveServers() {
	b, _ := json.MarshalIndent(servers, "", "  ")
	if err := os.WriteFile(serversPath, b, 0o644); err != nil {
		log.Printf("save %s: %v", serversPath, err)
	}
}

func isWhitelisted(guildID, username string) bool {
	username = strings.ToLower(username)
	srvMu.Lock()
	defer srvMu.Unlock()
	sd := servers[guildID]
	if sd == nil {
		return false
	}
	for _, u := range sd.Whitelist {
		if u == username {
			return true
		}
	}
	return false
}

func addWhitelist(guildID, username string) error {
	username = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(username, "@")))
	srvMu.Lock()
	defer srvMu.Unlock()
	sd := servers[guildID]
	if sd == nil {
		sd = &serverData{}
		servers[guildID] = sd
	}
	for _, u := range sd.Whitelist {
		if u == username {
			return nil
		}
	}
	sd.Whitelist = append(sd.Whitelist, username)
	saveServers()
	return nil
}

func getOverride(guildID, kw string) (override, bool) {
	srvMu.Lock()
	defer srvMu.Unlock()
	sd := servers[guildID]
	if sd == nil {
		return override{}, false
	}
	ov, ok := sd.Overrides[kw]
	return ov, ok
}

// overrideName is the application-emoji name for a guild override. The underscore
// keeps it distinct from global default names (keywords are alphanumeric only).
func overrideName(guildID, kw string) string {
	sum := sha1.Sum([]byte(guildID + ":" + kw))
	return "ov_" + hex.EncodeToString(sum[:])[:20]
}

// setOverride pins kw -> url for one guild only: deletes the previous override
// emoji (if any), uploads the new image, and persists it. Global defaults and
// other guilds are untouched.
func setOverride(s *discordgo.Session, guildID, kw, url string) error {
	if old, ok := getOverride(guildID, kw); ok {
		if err := s.ApplicationEmojiDelete(appID, old.ID); err != nil {
			return fmt.Errorf("delete old emoji: %w", err)
		}
	}
	e, err := uploadEmoji(s, overrideName(guildID, kw), url)
	if err != nil {
		return err
	}
	srvMu.Lock()
	sd := servers[guildID]
	if sd == nil {
		sd = &serverData{}
		servers[guildID] = sd
	}
	if sd.Overrides == nil {
		sd.Overrides = map[string]override{}
	}
	sd.Overrides[kw] = override{URL: url, ID: e.id, Animated: e.animated}
	saveServers()
	srvMu.Unlock()
	return nil
}

// loadAppEmojis caches emojis already uploaded by this app so we never re-upload.
func loadAppEmojis(s *discordgo.Session) {
	list, err := s.ApplicationEmojis(appID)
	if err != nil {
		log.Printf("load app emojis: %v", err)
		return
	}
	for _, e := range list {
		if strings.Contains(e.Name, "_") {
			continue // per-guild override, tracked in servers.json
		}
		appEmojis[strings.ToLower(e.Name)] = appEmoji{id: e.ID, animated: e.Animated}
	}
	log.Printf("loaded %d application emojis", len(list))
}

func isAdmin(s *discordgo.Session, m *discordgo.MessageCreate) bool {
	perms, err := s.UserChannelPermissions(m.Author.ID, m.ChannelID)
	return err == nil && perms&discordgo.PermissionAdministrator != 0
}

func handleCommand(s *discordgo.Session, m *discordgo.MessageCreate) {
	fields := strings.Fields(m.Content)
	if len(fields) < 2 {
		s.ChannelMessageSend(m.ChannelID, "commands: `"+cmdPrefix+" set <name> <url>`, `"+cmdPrefix+" allow <username>`")
		return
	}
	switch fields[1] {
	case "set":
		handleSet(s, m, fields)
	case "allow":
		handleAllow(s, m, fields)
	default:
		s.ChannelMessageSend(m.ChannelID, "unknown command — try `"+cmdPrefix+" set` or `"+cmdPrefix+" allow`")
	}
}

func handleSet(s *discordgo.Session, m *discordgo.MessageCreate, fields []string) {
	if !isWhitelisted(m.GuildID, m.Author.Username) && !isAdmin(s, m) {
		s.ChannelMessageSend(m.ChannelID, "you're not allowed — ask an admin to `"+cmdPrefix+" allow "+m.Author.Username+"`")
		return
	}
	name, url, ok := parseSet(fields)
	if !ok {
		s.ChannelMessageSend(m.ChannelID, "usage: `"+cmdPrefix+" set <name> <https-png-or-gif-url>` — name letters/numbers (2-64), https only")
		return
	}
	if err := setOverride(s, m.GuildID, name, url); err != nil {
		reportErr(s, m.ChannelID, "could not set "+name, err)
		return
	}
	s.ChannelMessageDelete(m.ChannelID, m.ID) // tidy the command away
	s.ChannelMessageSend(m.ChannelID, "✅ `:"+name+":` is now "+url+" on this server")
}

func handleAllow(s *discordgo.Session, m *discordgo.MessageCreate, fields []string) {
	if !isAdmin(s, m) {
		s.ChannelMessageSend(m.ChannelID, "only server admins can manage the whitelist")
		return
	}
	if len(fields) != 3 {
		s.ChannelMessageSend(m.ChannelID, "usage: `"+cmdPrefix+" allow <username>`")
		return
	}
	user := strings.TrimPrefix(fields[2], "@")
	if err := addWhitelist(m.GuildID, user); err != nil {
		reportErr(s, m.ChannelID, "could not update whitelist", err)
		return
	}
	s.ChannelMessageSend(m.ChannelID, "✅ `"+user+"` can now manage emotes on this server")
}

func onMessage(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author.Bot || m.WebhookID != "" || m.GuildID == "" {
		return // ignore bots, our own webhook posts, and DMs (no server = no override scope)
	}
	if m.Content == cmdPrefix || strings.HasPrefix(m.Content, cmdPrefix+" ") {
		handleCommand(s, m)
		return
	}
	content, changed := replaceTags(m.Content, func(kw string) (string, bool) {
		return emojiTag(s, m.GuildID, kw)
	})
	if !changed {
		return
	}
	if err := s.ChannelMessageDelete(m.ChannelID, m.ID); err != nil {
		log.Printf("delete: %v", err) // missing Manage Messages perm → original stays
	}

	whChannel, threadID := webhookTarget(s, m.ChannelID)
	id, token, err := webhookFor(s, whChannel)
	if err != nil {
		reportErr(s, m.ChannelID, "could not create webhook (does Geki have Manage Webhooks?)", err)
		return
	}
	// threadID == "" outside threads makes this a plain webhook execute.
	if _, err := s.WebhookThreadExecute(id, token, false, threadID, &discordgo.WebhookParams{
		Content:   replyPrefix(m) + content,
		Username:  displayName(m.Message),
		AvatarURL: m.Author.AvatarURL("128"),
	}); err != nil {
		reportErr(s, m.ChannelID, "could not post message", err)
	}
}

// webhookTarget maps a message channel to where its webhook lives and the thread
// to post into. Webhooks can't exist on a thread (forum posts are threads), so we
// use the parent channel + the thread id. Plain channels return (channelID, "").
func webhookTarget(s *discordgo.Session, channelID string) (whChannel, threadID string) {
	ch, err := s.State.Channel(channelID)
	if err != nil {
		ch, err = s.Channel(channelID) // not cached → fetch
	}
	if err == nil && ch.IsThread() {
		return ch.ParentID, channelID
	}
	return channelID, ""
}

// reportErr logs to the console and posts the error to the channel.
func reportErr(s *discordgo.Session, channelID, what string, err error) {
	log.Printf("%s: %v", what, err)
	s.ChannelMessageSend(channelID, "⚠️ Geki: "+what+": "+err.Error())
}

func displayName(m *discordgo.Message) string {
	switch {
	case m.Member != nil && m.Member.Nick != "":
		return m.Member.Nick // server nickname
	case m.Author != nil && m.Author.GlobalName != "":
		return m.Author.GlobalName
	case m.Author != nil:
		return m.Author.Username
	default:
		return "someone"
	}
}

// replyPrefix mimics Discord's reply bar with a subtext line + jump link back to
// the original. Webhooks can't post native replies, so this is the closest we get.
// Empty when the message isn't a reply.
func replyPrefix(m *discordgo.MessageCreate) string {
	r := m.MessageReference
	if r == nil || r.MessageID == "" {
		return ""
	}
	gid, cid := r.GuildID, r.ChannelID
	if gid == "" {
		gid = m.GuildID
	}
	if cid == "" {
		cid = m.ChannelID
	}
	who := "a message"
	if m.ReferencedMessage != nil {
		who = "@" + displayName(m.ReferencedMessage)
	}
	return fmt.Sprintf("-# ↪ [replying to %s](https://discord.com/channels/%s/%s/%s)\n", who, gid, cid, r.MessageID)
}

// webhookFor returns a Geki webhook for the channel, reusing or creating one.
// ponytail: tolerates a rare double-create under a race; harmless and self-heals on reuse.
func webhookFor(s *discordgo.Session, channelID string) (id, token string, err error) {
	whMu.Lock()
	w, ok := webhooks[channelID]
	whMu.Unlock()
	if ok {
		return w[0], w[1], nil
	}

	if list, err := s.ChannelWebhooks(channelID); err == nil {
		for _, h := range list {
			if h.Name == webhookName && h.Token != "" {
				id, token = cacheWebhook(channelID, h.ID, h.Token)
				return id, token, nil
			}
		}
	}
	h, err := s.WebhookCreate(channelID, webhookName, "")
	if err != nil {
		return "", "", err
	}
	id, token = cacheWebhook(channelID, h.ID, h.Token)
	return id, token, nil
}

func cacheWebhook(channelID, id, token string) (string, string) {
	whMu.Lock()
	webhooks[channelID] = [2]string{id, token}
	whMu.Unlock()
	return id, token
}

func main() {
	token := os.Getenv("DISCORD_TOKEN")
	if token == "" {
		log.Fatal("set DISCORD_TOKEN")
	}
	loadServers()

	dg, err := discordgo.New("Bot " + token)
	if err != nil {
		log.Fatal(err)
	}
	dg.Identify.Intents = discordgo.IntentsGuilds | discordgo.IntentsGuildMessages | discordgo.IntentMessageContent
	dg.AddHandler(onMessage)

	if err := dg.Open(); err != nil {
		log.Fatal(err)
	}
	defer dg.Close()

	appID = dg.State.User.ID // for bots, the application ID equals the bot user ID
	loadAppEmojis(dg)
	log.Println("Geki is running. Ctrl-C to stop.")

	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM)
	<-sc
}

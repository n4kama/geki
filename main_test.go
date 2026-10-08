package main

import (
	"net"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

func TestReplyPrefix(t *testing.T) {
	// not a reply → empty
	if got := replyPrefix(&discordgo.MessageCreate{Message: &discordgo.Message{}}); got != "" {
		t.Errorf("non-reply should be empty, got %q", got)
	}
	// reply with a known referenced author → subtext line + jump link
	m := &discordgo.MessageCreate{Message: &discordgo.Message{
		ChannelID:         "C",
		MessageReference:  &discordgo.MessageReference{MessageID: "M", ChannelID: "C", GuildID: "G"},
		ReferencedMessage: &discordgo.Message{Author: &discordgo.User{Username: "bob"}},
	}}
	want := "-# ↪ [replying to @bob](https://discord.com/channels/G/C/M)\n"
	if got := replyPrefix(m); got != want {
		t.Errorf("replyPrefix = %q, want %q", got, want)
	}
}

func TestReplaceTags(t *testing.T) {
	// Fake resolver: only "kappa" and "madge" exist; kappa is animated.
	resolve := func(kw string) (string, bool) {
		switch kw {
		case "kappa":
			return "<a:kappa:1>", true
		case "madge":
			return "<:madge:2>", true
		}
		return "", false
	}
	cases := []struct {
		in      string
		want    string
		changed bool
	}{
		{":madge:", "<:madge:2>", true},                            // whole message
		{"hello :madge: world", "hello <:madge:2> world", true},    // middle
		{":kappa: at the start", "<a:kappa:1> at the start", true}, // start, animated
		{"at the end :madge:", "at the end <:madge:2>", true},      // end
		{":KAPPA: is case-insensitive", "<a:kappa:1> is case-insensitive", true},
		{"two :madge::kappa: tags", "two <:madge:2><a:kappa:1> tags", true},
		{"just text", "just text", false},                       // nothing to do
		{"unknown :nope: stays", "unknown :nope: stays", false}, // unresolved tag kept
		{"mixed :madge: and :nope:", "mixed <:madge:2> and :nope:", true},
		{"keep <:madge:999> native", "keep <:madge:999> native", false}, // server's own emoji untouched
		{"keep <a:kappa:9> native", "keep <a:kappa:9> native", false},   // animated native emoji untouched
		{"native <:madge:9> + tag :madge:", "native <:madge:9> + tag <:madge:2>", true},
	}
	for _, c := range cases {
		got, changed := replaceTags(c.in, resolve)
		if got != c.want || changed != c.changed {
			t.Errorf("replaceTags(%q) = (%q,%v), want (%q,%v)", c.in, got, changed, c.want, c.changed)
		}
	}
}

func TestRewriteInsta(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"https://www.instagram.com/reel/abc/", "https://www.kkinstagram.com/reel/abc/", true},
		{"look https://instagram.com/reel/xyz nice", "look https://kkinstagram.com/reel/xyz nice", true},
		{"https://www.INSTAGRAM.com/reel/abc", "https://www.kkinstagram.com/reel/abc", true},
		{"https://instagram.com/p/abc", "https://instagram.com/p/abc", false},                   // post, not a reel
		{"https://www.kkinstagram.com/reel/abc", "https://www.kkinstagram.com/reel/abc", false}, // already rewritten
		{"just text", "just text", false},
	}
	for _, c := range cases {
		got, ok := rewriteInsta(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("rewriteInsta(%q) = (%q,%v), want (%q,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestRewriteTiktok(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"https://www.tiktok.com/@user/video/123", "https://d.tnktok.com/@user/video/123", true},
		{"short https://vm.tiktok.com/abc/", "short https://d.tnktok.com/abc/", true},
		{"https://vt.TIKTOK.com/xyz", "https://d.tnktok.com/xyz", true},
		{"bare tiktok.com/@u/video/1 link", "bare d.tnktok.com/@u/video/1 link", true},
		{"https://d.tnktok.com/@u/video/1", "https://d.tnktok.com/@u/video/1", false}, // already rewritten
		{"a notiktok.com/page url", "a notiktok.com/page url", false},                 // not a tiktok host
		{"just text", "just text", false},
	}
	for _, c := range cases {
		got, ok := rewriteTiktok(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("rewriteTiktok(%q) = (%q,%v), want (%q,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestRewriteTwitter(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"https://x.com/u/status/1", "https://fixupx.com/u/status/1", true},
		{"https://twitter.com/u/status/1", "https://fxtwitter.com/u/status/1", true},
		{"https://mobile.twitter.com/u/status/1", "https://fxtwitter.com/u/status/1", true},
		{"https://www.X.com/u/status/1", "https://fixupx.com/u/status/1", true},
		{"https://fixupx.com/u/status/1", "https://fixupx.com/u/status/1", false}, // already rewritten
		{"visit fox.com and max.com", "visit fox.com and max.com", false},         // not x.com
		{"just text", "just text", false},
	}
	for _, c := range cases {
		got, ok := rewriteTwitter(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("rewriteTwitter(%q) = (%q,%v), want (%q,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestRewriteReddit(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"https://www.reddit.com/r/x/comments/1/t/", "https://vxreddit.com/r/x/comments/1/t/", true},
		{"https://old.reddit.com/r/x", "https://vxreddit.com/r/x", true},
		{"https://vxreddit.com/r/x", "https://vxreddit.com/r/x", false}, // already rewritten
		{"just text", "just text", false},
	}
	for _, c := range cases {
		got, ok := rewriteReddit(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("rewriteReddit(%q) = (%q,%v), want (%q,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestRoastPrompt(t *testing.T) {
	self := "999"
	u := func(id, name string) *discordgo.User { return &discordgo.User{ID: id, Username: name} }
	cases := []struct {
		content        string
		mentions       []*discordgo.User
		prompt, target string
	}{
		// just @Geki → aimed at the pinger, no target
		{"<@999> roast me", []*discordgo.User{u("999", "Geki")}, "roast me", ""},
		// tell @Bob something → address Bob, ping Bob
		{"<@999> tell <@111> he is a bad player", []*discordgo.User{u("999", "Geki"), u("111", "Bob")}, "tell @Bob he is a bad player", "111"},
		// nickname mention form + several people → first non-Geki is the target
		{"<@!999> hi <@111> and <@222>", []*discordgo.User{u("999", "Geki"), u("111", "Bob"), u("222", "Al")}, "hi @Bob and @Al", "111"},
	}
	for _, c := range cases {
		prompt, target := roastPrompt(c.content, self, c.mentions)
		if prompt != c.prompt || target != c.target {
			t.Errorf("roastPrompt(%q) = (%q,%q), want (%q,%q)", c.content, prompt, target, c.prompt, c.target)
		}
	}
}

func TestStripMention(t *testing.T) {
	cases := []struct{ in, want string }{
		{"<@123> roast me", "roast me"},
		{"<@!123> nickname mention", "nickname mention"},
		{"hey <@123> what do you think", "hey  what do you think"},
		{"<@123>", ""},
		{"no mention here", "no mention here"},
	}
	for _, c := range cases {
		if got := strings.TrimSpace(mentionRe.ReplaceAllString(c.in, "")); got != strings.TrimSpace(c.want) {
			t.Errorf("strip(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestPickURL(t *testing.T) {
	files := []emoteFile{
		{Name: "2x.gif", Format: "GIF", Height: 64},
		{Name: "4x.gif", Format: "GIF", Height: 128},
		{Name: "4x.png", Format: "PNG", Height: 128},
		{Name: "4x.webp", Format: "WEBP", Height: 128},
	}
	host := "//cdn.7tv.app/emote/abc"

	if got := pickURL(true, host, files); got != "https://cdn.7tv.app/emote/abc/4x.gif" {
		t.Errorf("animated: got %q", got)
	}
	if got := pickURL(false, host, files); got != "https://cdn.7tv.app/emote/abc/4x.png" {
		t.Errorf("static: got %q", got)
	}
	if got := pickURL(false, host, files[:1]); got != "" {
		t.Errorf("no match should be empty, got %q", got)
	}
}

func TestParseSet(t *testing.T) {
	cases := []struct {
		in, name, url string
		ok            bool
	}{
		{"!geki set Madge https://x/y.png", "madge", "https://x/y.png", true},
		{"!geki set catkiss https://x/y.gif", "catkiss", "https://x/y.gif", true},
		{"!geki set madge http://x/y.png", "", "", false},    // http not allowed
		{"!geki set madge", "", "", false},                   // missing url
		{"!geki set madge a b", "", "", false},               // too many fields
		{"!geki allow madge https://x/y.png", "", "", false}, // not a set command
		{"!nope set madge https://x/y.png", "", "", false},   // wrong prefix
		{"!geki set mad_ge https://x/y.png", "", "", false},  // underscore not allowed
		{"!geki set x https://x/y.png", "", "", false},       // name too short
		{"!geki set madge ftp://x/y.gif", "", "", false},     // bad scheme
		{"hello world", "", "", false},                       // not a command
	}
	for _, c := range cases {
		name, url, ok := parseSet(strings.Fields(c.in))
		if name != c.name || url != c.url || ok != c.ok {
			t.Errorf("parseSet(%q) = (%q,%q,%v), want (%q,%q,%v)", c.in, name, url, ok, c.name, c.url, c.ok)
		}
	}
}

func TestIsPublicIP(t *testing.T) {
	blocked := []string{"127.0.0.1", "10.0.0.5", "192.168.1.1", "172.16.0.1", "169.254.1.1", "0.0.0.0", "::1", "fc00::1", "224.0.0.1"}
	for _, s := range blocked {
		if isPublicIP(net.ParseIP(s)) {
			t.Errorf("isPublicIP(%s) = true, want false", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"} {
		if !isPublicIP(net.ParseIP(s)) {
			t.Errorf("isPublicIP(%s) = false, want true", s)
		}
	}
}

func TestOverrideName(t *testing.T) {
	valid := regexp.MustCompile(`^[a-z0-9_]{2,32}$`)
	a := overrideName("111", "madge")
	if !valid.MatchString(a) {
		t.Errorf("invalid emoji name %q", a)
	}
	if overrideName("111", "madge") != a {
		t.Error("should be deterministic")
	}
	// distinct per guild and per keyword (so two servers' overrides never collide)
	if overrideName("222", "madge") == a || overrideName("111", "kappa") == a {
		t.Error("names should differ per guild/keyword")
	}
}

func TestTierCandidates(t *testing.T) {
	got := tierCandidates("https://cdn.7tv.app/emote/abc/2x.gif")
	want := []string{
		"https://cdn.7tv.app/emote/abc/4x.gif",
		"https://cdn.7tv.app/emote/abc/3x.gif",
		"https://cdn.7tv.app/emote/abc/2x.gif",
		"https://cdn.7tv.app/emote/abc/1x.gif",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("tierCandidates = %v, want %v", got, want)
	}
	if got := tierCandidates("https://example.com/pic.png"); len(got) != 1 || got[0] != "https://example.com/pic.png" {
		t.Errorf("non-7TV url should pass through, got %v", got)
	}
}

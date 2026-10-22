package slack

import "testing"

func TestThreadInputCommandIsExplicit(t *testing.T) {
	for _, tc := range []struct{ text, want string }{
		{"!aside discussion", "aside"}, {"!aside", "aside"}, {" <@bot> !aside mention is ignored", "aside"},
		{"!mute", "mute"}, {"<@bot> !unmute", "unmute"}, {" !mute\n", "mute"},
		{"!muted", ""}, {"!mute please", ""}, {"please !mute", ""}, {"!mute <@bot>", ""},
		{"`!mute`", ""}, {"> !mute", ""}, {"```\n!mute\n```", ""}, {"&gt; !mute", ""},
		{"<@someone> !mute", ""}, {"<@bot> > !mute", ""}, {"<@bot> `!aside`", ""},
	} {
		t.Run(tc.text, func(t *testing.T) {
			if got := threadInputCommand(tc.text, "bot"); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

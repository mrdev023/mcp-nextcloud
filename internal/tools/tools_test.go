package tools

import "testing"

func TestHTMLToText(t *testing.T) {
	in := `<p>Hi&nbsp;<b>there</b></p><script>evil()</script><ul><li>a</li><li>b</li></ul>`
	got := htmlToText(in)
	want := "Hi there\n\n- a\n\n- b"
	if got != want {
		t.Errorf("htmlToText = %q, want %q", got, want)
	}
	if s, cut := truncate("héllo", 2); s != "hé…" || !cut {
		t.Errorf("truncate = %q %v", s, cut)
	}
}

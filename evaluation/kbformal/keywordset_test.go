package kbformal

import "testing"

func TestAnchorWindowSkipsSpaceAndPunctuation(t *testing.T) {
	cases := map[string]string{
		"一二三四五六七八":     "五六七八",
		"甲乙丙丁，戊己 庚辛壬癸": "庚辛壬癸",
		"ABCD，。、；":     "ABCD", // nothing usable after the midpoint: first window from the start
	}
	for text, want := range cases {
		got, err := anchorWindow(text)
		if err != nil || got != want {
			t.Fatalf("anchorWindow(%q) = %q, %v; want %q", text, got, err, want)
		}
	}
	if _, err := anchorWindow("，。 、；"); err == nil {
		t.Fatal("a chunk without a usable window produced an anchor")
	}
}

func TestFrequentBigramsCountChunksOnceAndBreakTiesByCodePoint(t *testing.T) {
	texts := []string{"合同合同合同", "合同条款", "条款", "ab合同"}
	got := frequentBigrams(texts, 3)
	if len(got) != 3 || got[0].text != "合同" || got[0].chunks != 3 || got[1].text != "条款" || got[1].chunks != 2 {
		t.Fatalf("frequent bigrams = %+v", got)
	}
}

func TestAbsentKeywordsAreDeterministicAndAbsent(t *testing.T) {
	corpora := [][]string{{"任意文本"}, {"另一份语料"}}
	a, err := absentKeywords(corpora, 5)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := absentKeywords(corpora, 5)
	for i := range a {
		if a[i] != b[i] || len([]rune(a[i])) != 4 {
			t.Fatalf("absent keywords not deterministic 4-rune strings: %q %q", a, b)
		}
	}
}

package brand

import (
	"encoding/base64"
	"encoding/xml"
	"os"
	"strings"
	"testing"
)

func TestIconMatchesTheDocuments(t *testing.T) {
	docs, err := os.ReadFile("../../docs/assets/moorage-icon.svg")
	if err != nil {
		t.Fatal(err)
	}
	if string(docs) != icon {
		t.Error("icon.svg differs from docs/assets/moorage-icon.svg: copy the one that changed over the other")
	}
	if err := xml.Unmarshal([]byte(icon), new(any)); err != nil {
		t.Errorf("icon.svg is not XML: %v", err)
	}
}

func TestFaviconURL(t *testing.T) {
	data, ok := strings.CutPrefix(string(FaviconURL()), "data:image/svg+xml;base64,")
	if !ok {
		t.Fatalf("FaviconURL = %.40s…", FaviconURL())
	}
	if svg, err := base64.StdEncoding.DecodeString(data); err != nil || string(svg) != icon {
		t.Errorf("the favicon is not the icon: %v", err)
	}
}

package store

import (
	"testing"

	"github.com/tattsun/collie/internal/model"
)

func TestTimelineFold(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_ = st.AppendItem("s", &model.Item{ID: "a", Kind: "text", Text: "one"})
	_ = st.AppendItem("s", &model.Item{ID: "b", Kind: "text", Text: "two"})
	_ = st.AppendItem("s", &model.Item{ID: "a", Kind: "text", Text: "one, final"})
	items, lines, err := st.LoadItems("s")
	if err != nil {
		t.Fatal(err)
	}
	if lines != 3 || len(items) != 2 {
		t.Fatalf("lines=%d items=%d", lines, len(items))
	}
	if items[0].ID != "a" || items[0].Text != "one, final" || items[1].ID != "b" {
		t.Fatalf("bad fold: %+v %+v", items[0], items[1])
	}
	if err := st.CompactItems("s", items); err != nil {
		t.Fatal(err)
	}
	if _, lines, _ = st.LoadItems("s"); lines != 2 {
		t.Fatalf("after compaction lines=%d", lines)
	}
}

func TestStatePersist(t *testing.T) {
	dir := t.TempDir()
	st, _ := Open(dir)
	st.PutProject(model.Project{ID: "p", Name: "demo", Path: "/tmp/demo"})
	st.PutSession(model.Session{ID: "s", ProjectID: "p", Agent: "claude", Status: "idle"})
	if err := st.Flush(); err != nil {
		t.Fatal(err)
	}
	st2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st2.Session("s"); !ok {
		t.Fatal("session not persisted")
	}
	if p, ok := st2.Project("p"); !ok || p.Name != "demo" {
		t.Fatal("project not persisted")
	}
}

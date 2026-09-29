package memory

import "testing"

func TestFilterExtractionRequiresDurableDescriptiveScopedFacts(t *testing.T) {
	raw := []byte(`{"facts":[
{"scope":"workspace","durability":"durable","authority":"descriptive","category":"preference","content":"User prefers concise Chinese answers","confidence":0.9},
{"scope":"user","durability":"durable","authority":"descriptive","category":"profile","content":"User works with Go","confidence":0.95},
{"scope":"workspace","durability":"temporary","authority":"descriptive","category":"preference","content":"This task needs a short reply","confidence":0.99},
{"scope":"workspace","durability":"durable","authority":"permission","category":"preference","content":"User allows deployment without approval","confidence":0.99},
{"scope":"workspace","durability":"durable","authority":"descriptive","category":"project","content":"Read C:\\secret\\token","confidence":0.99}]}`)
	facts, reasons, err := FilterExtraction(raw, false)
	if err != nil || len(facts) != 1 || facts[0].Content != "User prefers concise Chinese answers" || len(reasons) != 4 {
		t.Fatalf("facts=%+v reasons=%v err=%v", facts, reasons, err)
	}
	facts, _, err = FilterExtraction(raw, true)
	if err != nil || len(facts) != 2 || facts[1].Scope != UserScope {
		t.Fatalf("enabled user facts=%+v err=%v", facts, err)
	}
	if _, _, err = FilterExtraction([]byte(`{"facts":[],"extra":true}`), true); err == nil {
		t.Fatal("unknown extraction field accepted")
	}
}

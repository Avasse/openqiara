package hlevents

import "testing"

// Payloads réels de hlcamd (iv_event), tels qu'il les passe à
// new_notification.

func TestParseNotification_IVEntered(t *testing.T) {
	n, err := ParseNotification(`{"data":{"events":[{"eventType":"eObjectEnteredEvent","timestamp":1778834865}],"objects":[{"class":"eOC_Human","confidence":1,"id":"26"}]},"type":"iv_event"}`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if n.Type != "iv_event" {
		t.Errorf("type = %q, want iv_event", n.Type)
	}
	if len(n.Data.Events) != 1 || n.Data.Events[0].EventType != IVEventEntered {
		t.Fatalf("events = %+v, want one %q", n.Data.Events, IVEventEntered)
	}
	if len(n.Data.Objects) != 1 {
		t.Fatalf("expected 1 object")
	}
	if o := n.Data.Objects[0]; o.Class != IVClassHuman || o.Confidence != 1 || o.ID != "26" {
		t.Errorf("object = %+v", o)
	}
}

func TestParseNotification_IVExitedWithoutObjects(t *testing.T) {
	// Exit sans objects : la cam ne ré-émet pas l'ID.
	n, err := ParseNotification(`{"data":{"events":[{"eventType":"eObjectExitedEvent","timestamp":1778834886}]},"type":"iv_event"}`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if n.Data.Events[0].EventType != IVEventExited {
		t.Errorf("eventType = %q", n.Data.Events[0].EventType)
	}
	if len(n.Data.Objects) != 0 {
		t.Errorf("expected no objects, got %d", len(n.Data.Objects))
	}
}

func TestParseNotification_IVClassifiedOnly(t *testing.T) {
	// Classified pur sans events, juste un object remis à jour.
	n, err := ParseNotification(`{"data":{"objects":[{"class":"eOC_Human","confidence":1,"id":"26"}]},"type":"iv_event"}`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(n.Data.Events) != 0 || len(n.Data.Objects) != 1 {
		t.Errorf("events %d objects %d, want 0 and 1", len(n.Data.Events), len(n.Data.Objects))
	}
}

func TestParseNotification_Garbage(t *testing.T) {
	if _, err := ParseNotification("not json"); err == nil {
		t.Fatal("garbage accepted")
	}
}

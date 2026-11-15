package workflow

import (
	"encoding/json"
	"testing"
)

func TestPriorityDefaultsAndValidation(t *testing.T) {
	for _, trigger := range []string{TriggerTask, TriggerManual, TriggerEmail, TriggerVoiceMemo} {
		for _, priority := range []string{"", "normal", "immediate", "invalid"} {
			d := Starting
			d.Host, d.Trigger = "host-1", trigger
			if trigger == TriggerEmail {
				d.Email = &EmailFilter{From: []string{"sender@example.com"}}
			}
			if trigger == TriggerVoiceMemo {
				d.VoiceMemo = &VoiceMemoFilter{}
			}
			b, _ := json.Marshal(d)
			var fields map[string]any
			json.Unmarshal(b, &fields)
			fields["priority"] = priority
			b, _ = json.Marshal(fields)
			got, err := Parse(b)
			if priority == "invalid" {
				if err == nil {
					t.Error("accepted invalid priority")
				}
				continue
			}
			if err != nil {
				t.Fatalf("%s/%s: %v", trigger, priority, err)
			}
			b, _ = json.Marshal(got)
			json.Unmarshal(b, &fields)
			want := priority
			if want == "" {
				want = "normal"
				if trigger == TriggerEmail || trigger == TriggerVoiceMemo {
					want = "immediate"
				}
			}
			if got.Priority != priority {
				t.Errorf("configured priority changed: %q -> %q", priority, got.Priority)
			}
			if got.EffectivePriority() != want {
				t.Errorf("%s/%s: priority = %v, want %s", trigger, priority, got.EffectivePriority(), want)
			}
		}
	}
}

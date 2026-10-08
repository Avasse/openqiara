package web

import (
	"testing"

	"github.com/caligone/openqiara/internal/config"
)

func TestValidateBrokerURL(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"vide = champ non modifié", "", false},
		{"host:port nu", "192.168.1.10:1883", false},
		{"host sans port", "broker.local", false},
		{"schéma tcp", "tcp://192.168.1.10:1883", false},
		{"schéma ssl", "ssl://broker.local:8883", false},
		{"schéma ws", "ws://broker.local:9001", false},
		{"schéma wss", "wss://broker.local:443", false},
		{"schéma mqtts", "mqtts://broker.local:8883", false},
		{"IPv6 avec port", "tcp://[fd00::1]:1883", false},
		{"schéma inconnu", "http://broker.local:1883", true},
		{"hôte manquant après schéma", "tcp://", true},
		{"port non numérique", "tcp://broker.local:mqtt", true},
		{"port hors bornes", "tcp://broker.local:70000", true},
		{"port zéro", "tcp://broker.local:0", true},
		{"espace interne", "tcp://broker local:1883", true},
		{"retour ligne injecté", "tcp://broker\nlocal", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateBrokerURL(tc.in)
			if (err != nil) != tc.wantErr {
				t.Errorf("validateBrokerURL(%q) err = %v, wantErr %v", tc.in, err, tc.wantErr)
			}
		})
	}
}

func TestValidateHomeKitPin(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"vide = inchangé", "", false},
		{"défaut openqiara", "00102003", false},
		{"8 chiffres quelconques", "31415926", false},
		{"trop court", "1234567", true},
		{"trop long", "123456789", true},
		{"avec tirets", "031-45-926", true},
		{"lettres", "0010200a", true},
		{"trivial répété", "11111111", true},
		{"trivial séquentiel", "12345678", true},
		{"trivial séquentiel inversé", "87654321", true},
		{"trivial zéros", "00000000", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateHomeKitPin(tc.in)
			if (err != nil) != tc.wantErr {
				t.Errorf("validateHomeKitPin(%q) err = %v, wantErr %v", tc.in, err, tc.wantErr)
			}
		})
	}
}

// TestHomeKitPinMasking : le setup code est un secret d'appairage (qui le lit
// peut rattacher un contrôleur et piloter l'alarme), pas un champ de config
// lisible. Il doit être masqué en lecture, et la valeur masquée renvoyée en
// écriture ne doit jamais devenir le PIN réel.
func TestHomeKitPinMasking(t *testing.T) {
	masked := maskHomeKit(config.HomeKitConfig{Enabled: true, Pin: "31415926", Name: "Cam"})
	if masked.Pin != maskedSecret {
		t.Errorf("Pin = %q, want %q", masked.Pin, maskedSecret)
	}
	if masked.Name != "Cam" || !masked.Enabled {
		t.Errorf("maskHomeKit a altéré autre chose que Pin: %+v", masked)
	}
	// Pas de PIN configuré → rien à masquer, on ne fabrique pas un faux secret.
	if got := maskHomeKit(config.HomeKitConfig{}).Pin; got != "" {
		t.Errorf("Pin vide masqué en %q, want \"\"", got)
	}
	// Le round-trip lecture→écriture doit passer la validation.
	if err := validateHomeKitPin(maskedSecret); err != nil {
		t.Errorf("valeur masquée rejetée en écriture: %v", err)
	}
}

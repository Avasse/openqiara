package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/caligone/openqiara/internal/camera"
	"github.com/caligone/openqiara/internal/config"
)

// sirenAPI est le sous-ensemble de camera.Client dont sirenController a besoin.
// Découpé pour faciliter le mock en tests sans recréer toute l'interface Client.
type sirenAPI interface {
	CachedSensors() []camera.Sensor
	TriggerSiren(ctx context.Context, sensorID int) error
	TriggerSirenAlarm(ctx context.Context, sensorID int, duration time.Duration) error
	StopSiren(ctx context.Context, sensorID int) error
}

// sirenBeeper joue les bips courts de la sirène (armement, désarmement).
// Implémenté par camera.RadioClient ; nil en mode fbxhome.
type sirenBeeper interface {
	SirenBeep(ctx context.Context, sensorID int, tone camera.SirenTone) error
}

// Une interface détectée par assertion de type échoue en silence si la
// signature diverge : les bips charmux n'ont jamais joué pour cette raison.
var _ sirenBeeper = (*camera.RadioClient)(nil)

// sirenController pilote la sirène physique en fonction des transitions
// d'état de la centrale d'alarme.
//
// Threading model :
//   - Handle() est appelé depuis le callback alarm engine (mode standalone)
//     ou depuis le handler MQTT (mode alarmo). Les deux peuvent venir de
//     goroutines distinctes ; un mutex interne protège sirenReady.
//   - Les commandes SRN sont lancées en goroutine pour ne pas bloquer le
//     caller (qui détient potentiellement un lock alarm engine). En tests,
//     synchronous=true exécute les commandes inline pour permettre des
//     assertions immédiates.
type sirenController struct {
	cam     sirenAPI
	beeper sirenBeeper // nil = mode fbxhome
	store   *config.Store
	logger  *slog.Logger
	ctx     context.Context

	mu          sync.Mutex
	sirenReady  bool
	synchronous bool // tests uniquement

	wg sync.WaitGroup // tests uniquement : permet d'attendre les goroutines
}

// newSirenController construit un controller pour le runtime (goroutines
// fire-and-forget, synchronous=false).
func newSirenController(ctx context.Context, cam sirenAPI, store *config.Store, logger *slog.Logger) *sirenController {
	sc := &sirenController{
		cam:    cam,
		store:  store,
		logger: logger,
		ctx:    ctx,
	}
	if b, ok := cam.(sirenBeeper); ok {
		sc.beeper = b
	}
	return sc
}

// Handle pilote la sirène pour une transition d'état alarme. Idempotent
// pour les transitions sans changement (newState == prevState).
func (s *sirenController) Handle(newState, prevState string) {
	if newState == prevState {
		return
	}

	s.mu.Lock()
	// L'alarm engine émet "disarmed" au boot — ne pas beep pour ça.
	if !s.sirenReady {
		s.sirenReady = true
		if newState == "disarmed" {
			s.mu.Unlock()
			return
		}
	}
	s.mu.Unlock()

	mode := s.store.Get().SirenSoundsMode()

	// "none" doit quand même couper un wail en cours (sinon l'utilisateur
	// n'a aucun moyen de l'arrêter après avoir toggle à none). Intercepte
	// aussi "triggered" pour killer le SRN avant qu'il monte en puissance.
	if mode == "none" {
		s.run(func() {
			if addr := s.findSRN(); addr != 0 {
				_ = s.cam.StopSiren(s.ctx, addr)
			}
		})
		return
	}

	switch newState {
	case "arming":
		if mode != "all" {
			return
		}
		// En mode fbxhome (sans charmux) le seul tone exposé par l'API
		// vendor est `test=true` qui déclenche un wail (≈1s audible avant
		// que le reboot_srn du disarm ne le coupe). Ce n'est pas un beep
		// court — skip plutôt que produire un faux beep.
		if s.beeper == nil {
			return
		}
		s.run(func() {
			addr := s.findSRN()
			if addr == 0 {
				return
			}
			s.logger.Info("siren: arming beep", "addr", addr)
			if err := s.beeper.SirenBeep(s.ctx, addr, camera.ToneArming); err != nil {
				s.logger.Error("siren: arming beep failed", "error", err)
			}
		})

	case "triggered":
		s.run(func() {
			addr := s.findSRN()
			if addr == 0 {
				return
			}
			wail := s.store.Get().WailDuration()
			s.logger.Warn("siren: ALARM TRIGGERED — firing wail", "addr", addr, "duration", wail)
			if err := s.cam.TriggerSirenAlarm(s.ctx, addr, wail); err != nil {
				s.logger.Error("siren: wail failed", "error", err)
			}
		})

	case "disarmed":
		s.run(func() {
			addr := s.findSRN()
			if addr == 0 {
				return
			}
			s.logger.Info("siren: disarm — sending stop", "addr", addr)
			if err := s.cam.StopSiren(s.ctx, addr); err != nil {
				s.logger.Error("siren: stop failed", "error", err)
			}
			if mode == "all" && s.beeper != nil {
				// Disarm beep — uniquement dispo comme tone dédié en charmux.
				// En mode fbxhome il n'y a pas d'équivalent court : skip.
				if err := s.beeper.SirenBeep(s.ctx, addr, camera.ToneDisarm); err != nil {
					s.logger.Error("siren: disarm beep failed", "error", err)
				}
			}
		})
	}
}

// run exécute fn en goroutine (runtime normal) ou inline (tests).
func (s *sirenController) run(fn func()) {
	if s.synchronous {
		fn()
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		fn()
	}()
}

func (s *sirenController) findSRN() int {
	for _, snap := range s.cam.CachedSensors() {
		if snap.Type == "SRN" {
			return snap.ID
		}
	}
	return 0
}

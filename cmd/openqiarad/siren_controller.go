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

// sirenController pilote la sirène physique en fonction des transitions
// d'état de la centrale d'alarme : wail au déclenchement, arrêt au
// désarmement. Pas de bip d'armement ni de désarmement : les trames de
// bip d'avril 2026 font passer la sirène par ses propres états (armée,
// délai), à trancher avec la question « qui porte les délais, openqiara
// ou la sirène ». Aucun mode ne les a jamais jouées.
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
	cam    sirenAPI
	store  *config.Store
	logger *slog.Logger
	ctx    context.Context

	mu          sync.Mutex
	sirenReady  bool
	synchronous bool // tests uniquement

	wg sync.WaitGroup // tests uniquement : permet d'attendre les goroutines
}

// newSirenController construit un controller pour le runtime (goroutines
// fire-and-forget, synchronous=false).
func newSirenController(ctx context.Context, cam sirenAPI, store *config.Store, logger *slog.Logger) *sirenController {
	return &sirenController{
		cam:    cam,
		store:  store,
		logger: logger,
		ctx:    ctx,
	}
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

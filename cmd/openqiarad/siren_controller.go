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
	BeepSiren(ctx context.Context, sensorID int) error
	TriggerSirenAlarm(ctx context.Context, sensorID int, duration time.Duration) error
	StopSiren(ctx context.Context, sensorID int) error
}

// Cadence des bips pendant les délais de la centrale, plus pressante au
// délai d'entrée : c'est le signal « désarme, sinon ça sonne ».
var beepEvery = map[string]time.Duration{
	"arming":  5 * time.Second,
	"pending": 2 * time.Second,
}

// maxBeeping borne une série de bips si l'état ne bouge plus (un délai
// Alarmo plus long que prévu, une transition perdue).
const maxBeeping = 10 * time.Minute

// sirenController pilote la sirène physique en fonction des transitions
// d'état de la centrale d'alarme : bips pendant les délais d'armement et
// d'entrée, wail au déclenchement, arrêt au désarmement.
//
// openqiara porte les délais, la sirène ne sert que de haut-parleur : les
// bips sont son discret très court (BeepSiren), pas les trames d'armement
// de fbxhome (55 04, 55 05 04 80) qui font tenir à la sirène son propre
// compte à rebours, à désynchroniser du nôtre.
//
// Threading model :
//   - Handle() est appelé depuis le callback alarm engine (mode standalone)
//     ou depuis le handler MQTT (mode alarmo), potentiellement sous le lock
//     de l'engine : il ne bloque pas. Les commandes passent par une file
//     unique, exécutée dans l'ordre des transitions (un stop ne doit pas
//     doubler un wail). En tests, synchronous=true les exécute inline.
//   - Une série de bips tourne dans sa propre goroutine ; chaque
//     transition l'arrête et attend qu'elle ait fini avant d'agir, pour
//     qu'aucun bip n'arrive après le wail.
type sirenController struct {
	cam    sirenAPI
	store  *config.Store
	logger *slog.Logger
	ctx    context.Context

	mu          sync.Mutex
	sirenReady  bool
	stopBeeps   func() // arrête la série de bips en cours et l'attend ; nil sinon
	synchronous bool   // tests uniquement
	every       map[string]time.Duration

	queue     chan func()
	startOnce sync.Once
}

// newSirenController construit un controller pour le runtime.
func newSirenController(ctx context.Context, cam sirenAPI, store *config.Store, logger *slog.Logger) *sirenController {
	return &sirenController{
		cam:    cam,
		store:  store,
		logger: logger,
		ctx:    ctx,
		every:  beepEvery,
		queue:  make(chan func(), 16),
	}
}

// Handle pilote la sirène pour une transition d'état alarme. Idempotent
// pour les transitions sans changement (newState == prevState).
func (s *sirenController) Handle(newState, prevState string) {
	if newState == prevState {
		return
	}

	s.mu.Lock()
	// L'alarm engine émet "disarmed" au boot — rien à faire pour ça.
	if !s.sirenReady {
		s.sirenReady = true
		if newState == "disarmed" {
			s.mu.Unlock()
			return
		}
	}
	s.mu.Unlock()

	mode := s.store.Get().SirenSoundsMode()

	s.run(func() {
		s.silence()
		addr := s.findSRN()
		if addr == 0 {
			return
		}
		// "none" doit quand même couper un wail en cours (sinon
		// l'utilisateur n'a aucun moyen de l'arrêter après avoir basculé à
		// none).
		if mode == "none" {
			_ = s.cam.StopSiren(s.ctx, addr)
			return
		}
		switch newState {
		case "arming", "pending":
			if mode == "all" {
				s.beep(addr, s.every[newState])
			}
		case "triggered":
			wail := s.store.Get().WailDuration()
			s.logger.Warn("siren: ALARM TRIGGERED — firing wail", "addr", addr, "duration", wail)
			if err := s.cam.TriggerSirenAlarm(s.ctx, addr, wail); err != nil {
				s.logger.Error("siren: wail failed", "error", err)
			}
		case "disarmed":
			s.logger.Info("siren: disarm — sending stop", "addr", addr)
			if err := s.cam.StopSiren(s.ctx, addr); err != nil {
				s.logger.Error("siren: stop failed", "error", err)
			}
		}
	})
}

// beep fait biper la sirène toutes les every, jusqu'à la transition
// suivante ou maxBeeping.
func (s *sirenController) beep(addr int, every time.Duration) {
	ctx, cancel := context.WithTimeout(s.ctx, maxBeeping)
	done := make(chan struct{})
	s.mu.Lock()
	s.stopBeeps = func() { cancel(); <-done }
	s.mu.Unlock()
	go func() {
		defer close(done)
		tick := time.NewTicker(every)
		defer tick.Stop()
		for {
			if err := s.cam.BeepSiren(ctx, addr); err != nil && ctx.Err() == nil {
				s.logger.Warn("siren: beep failed", "error", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
}

// silence arrête la série de bips en cours, s'il y en a une, et attend
// qu'elle ait fini.
func (s *sirenController) silence() {
	s.mu.Lock()
	stop := s.stopBeeps
	s.stopBeeps = nil
	s.mu.Unlock()
	if stop != nil {
		stop()
	}
}

// run exécute fn dans la file des commandes sirène (runtime normal) ou
// inline (tests).
func (s *sirenController) run(fn func()) {
	if s.synchronous {
		fn()
		return
	}
	s.startOnce.Do(func() {
		go func() {
			for {
				select {
				case fn := <-s.queue:
					fn()
				case <-s.ctx.Done():
					return
				}
			}
		}()
	})
	select {
	case s.queue <- fn:
	case <-s.ctx.Done():
	}
}

func (s *sirenController) findSRN() int {
	for _, snap := range s.cam.CachedSensors() {
		if snap.Type == "SRN" {
			return snap.ID
		}
	}
	return 0
}

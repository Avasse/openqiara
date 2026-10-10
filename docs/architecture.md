# Architecture

## Vue d'ensemble

OpenQiara est un **overlay** sur le rootfs stock de la caméra. Il ne re-flashe
pas le firmware. `openqiarad` est lui-même la passerelle radio : le daemon
constructeur `fbxhome` est arrêté au boot, et `openqiarad` parle directement au
MCU pour exposer les capteurs à Home Assistant / HomeKit, et fait tourner le
moteur d'alarme local (ou le bridge vers l'Alarmo de Home Assistant).

```
┌──────────────────────────────────────────────────────────────┐
│ Caméra Qiara                                                 │
│                                                              │
│  ┌────────────┐  UDP 8001/8003  ┌────────┐   ┌─────────────┐ │
│  │ openqiarad │◄───────────────►│charmux │◄─►│ MCU EZR32LG │ │
│  │            │                 │(vendor)│   │ radio Si446x│ │
│  │  ┌──────┐  │                 └────────┘   │  868MHz     │ │
│  │  │Web UI│  │                              └─────────────┘ │
│  │  └──────┘  │                                              │
│  └─────┬──────┘                                              │
│        │ :80                                                 │
└────────┼─────────────────────────────────────────────────────┘
         │ WiFi
         ▼
┌─────────────────┐     ┌──────────────────┐
│ Navigateur      │     │ Home Assistant   │
│ (setup/pairing) │     │ MQTT discovery   │
└─────────────────┘     │ Alarmo (optionnel)│
                        │ Bridge HomeKit   │
                        └──────────────────┘
```

## Passerelle radio

```
openqiarad → UDP 8001/8003 → charmux → MCU
```

`camera.RadioClient` confie la radio au moteur `internal/radio`, qui
reproduit fbxhome trame pour trame : il répond à chaque trame qui attend
une réponse (Z), pousse bytecode, heure, config et codes clavier quand le
capteur les demande, et traduit ses trames en événements. Rejoué sur les
logs de fbxhome en production (`cmd/radio-shadow`), il ne diffère d'aucune
trame. L'appairage (`domus.Pair`) ne fait que le handshake CTRL : le
capteur est ensuite provisionné comme après n'importe quel redémarrage. Le
premier capteur du type demandé qui passe en mode appairage est pris.

Le registre des capteurs est la config (`sensors[].radio` : adresse, UID,
index système). Au premier démarrage, il est importé de `/data/fbxhome.xml.*`
(`ImportFbxhomeRadio`) : chaque capteur garde son id, donc ses entités Home
Assistant et Alarmo. Le mode jour/nuit passe par `fbxbusctl set hlcamd
video_settings`, le volet par le canal charmux 8006/8007.

`camera_boot.sh` arrête fbxhome au boot, puis lance `openqiarad` : le
démarrage échoue tant que fbxhome tourne, car il tient les ports charmux.
`-log-level debug` trace chaque trame (sans les codes clavier).

La sirène reçoit la durée du wail dans la trame et s'arrête d'elle-même.
Pas de bip d'armement ni de désarmement : leurs trames font passer la sirène
par ses propres états, question ouverte.

Non géré : capteurs derrière un répéteur, redémarrage à distance de la sirène.

## Layout rootfs

La carte SD garde ses 3 partitions ; le rootfs stock est largement inchangé.
`openqiarad` vit sur `/data` et est démarré par `boot.sh`, qui est crocheté
dans l'init existant.

| Partition | Montage | Contenu |
|-----------|-------|---------|
| mmcblk0p1 | / | Rootfs stock constructeur (kernel, drivers, charmux, uartboot, hlcamd, nginx ; fbxhome y reste mais est arrêté au boot) |
| mmcblk0p2 | /data | Binaire openqiarad, config persistante, état capteurs, état moteur d'alarme, log |
| mmcblk0p3 | /media | Stockage média (segments HLS, etc.) |

### Ce qu'on garde du stock
- Kernel Linux 4.9.84
- Modules kernel (WiFi `ssv6x5x`, capteurs caméra, etc.)
- Firmware MCU (`hlcam02_ctrl.bin`) flashé à chaque boot par `uartboot`
- Multiplexeur UART `charmux`
- `hlcamd` pour l'encodage vidéo (le HLS est servi par openqiarad)
- `nginx`
- SSH `dropbear`

### Ce qu'on ajoute
- `openqiarad` sur `/data` — le daemon qui chapeaute tout : discovery MQTT
  pour HA, bridge HomeKit, moteur d'alarme, web UI sur `:80`.
- `boot.sh` sur `/data` — arrête fbxhome, neutralise le reboot périodique
  (`fbxbusctl call watchdog_mcu set_timeout`), démarre le daemon
  (`/data/openqiarad -web :80 -log /data/openqiarad.log`).

OpenQiara ne remplace **pas** le système init, `hlconnman`, ni `nginx`.

## Structure du module Go

```
openqiara/
├── cmd/
│   ├── openqiarad/         # Daemon principal (tourne sur la caméra)
│   ├── openqiara-flash/    # Outil de flash SD (poste de dev)
│   ├── mcu-info/           # Lecteur d'info MCU (debug)
│   ├── charmux-test/       # Client charmux debug (debug)
│   ├── decode-frame/       # Décodeur de managed frame (debug)
│   ├── radio-shadow/       # Rejoue les logs fbxhome contre internal/radio (debug)
│   └── rtptest/            # Test pipeline SRTP/RTP (debug)
├── internal/
│   ├── alarm/              # Machine à états d'alarme autonome
│   │   └── engine.go       # Transitions d'état, armement Source-aware, timers, persistance
│   ├── camera/             # Interface client caméra + implémentations
│   │   ├── client.go       # Interface Client (Sensors, Pair, Events, SendPKT, SetShutter, TriggerSiren)
│   │   ├── radio.go        # RadioClient (openqiarad passerelle radio, via internal/radio)
│   │   ├── fbxhome_xml.go  # ImportFbxhomeRadio (import des capteurs de fbxhome.xml)
│   │   └── types.go        # Sensor, SensorEvent, etc.
│   ├── charmux/            # Client UDP charmux bas niveau
│   ├── radio/              # Moteur radio : réponses aux trames, provisioning capteurs
│   ├── config/             # Store de config JSON
│   ├── domus/              # Handshake d'appairage DomusRF
│   ├── mdns/               # Annonce mDNS (openqiara.local)
│   ├── mqtt/               # Publisher MQTT HA + discovery
│   ├── publisher/          # Abstraction Publisher (MQTT + HomeKit en parallèle)
│   ├── system/             # Helpers système (reboot, etc.)
│   └── web/                # Web UI + API REST + SSE
│       └── server.go       # Handlers HTTP, auth, proxy HLS, pipeline SRTP
└── web/
    └── static/
        └── index.html      # SPA (embarquée via embed.FS)
```

## Moteur d'alarme

La machine à états d'alarme autonome vit dans `internal/alarm/engine.go`.
Elle tourne dans deux modes :

- **`standalone`** : engine local, source de vérité de l'état alarme
- **`alarmo`** : engine local désactivé, HA Alarmo via MQTT est la source
  de vérité ; openqiarad écoute `alarmo/state` et publie sur
  `alarmo/command`

États : `disarmed`, `arming`, `armed_away`, `armed_night`, `pending`,
`triggered`.

### Armement Source-aware

Les commandes portent une `Source` (`Local` ou `Remote`) qui adapte le comportement :

- **`SourceLocal`** (KPD physique) : `disarmed → arming` pendant
  `arming_delay_seconds`, puis `arming → armed_*`. Pendant le délai,
  les events capteurs sont ignorés (grâce pour fermer la porte/sortir).
  À expiration, snapshot : tout capteur encore en alarme déclenche
  immédiatement `pending`.

- **`SourceRemote`** (HK / HA Alarmo / Web UI) : pas de délai d'armement
  (`disarmed → armed_*` direct), check immédiat post-arm : tout capteur
  déjà en alarme déclenche `pending` instantanément.

Cette asymétrie reflète l'usage : Local = user présent qui peut réagir,
Remote = user distant qui veut une protection stricte immédiate.

### Flow de déclenchement

```
armed_*  ──(alarme capteur)──►  pending  ──(timer)──►  triggered  ──(timer)──►  armed_*
   ▲                              │                        │
   └────────(désarmement à tout moment)─┴───────────────────┘
```

`pending` est la temporisation de grâce user (code clavier OK = disarm avant
`pending → triggered`). `triggered` joue le wail SRN pendant
`wail_duration_seconds`, puis retour à l'état armé précédent.

## Pattern Publisher

Les publishers synchronisent l'état avec les systèmes externes. Plusieurs publishers fonctionnent en parallèle :

```
                     ┌─── MQTTPublisher ──→ HA (broker MQTT)
Capteurs → Core ────┤
                     └─── HomeKitPublisher ──→ Apple Home / HA (mDNS)
```

Interface :
```go
type Publisher interface {
    Start(ctx, sensors, commands) error
    PublishSensorState(ctx, sensor) error
    PublishAlarmState(ctx, state) error
    Close() error
}
```

Le `CommandHandler` reçoit les commandes entrantes (arm/disarm, siren on/off) de n'importe quel publisher et les dispatch à tous les autres.

## API REST

Servie par `internal/web` sur le port `-web` (`:80` par défaut), sous le
préfixe `/api/v1`. Deux familles cohabitent :

- **Ressources** — `/sensors`, `/config`, `/alarm`, `/kpd`, `/status`,
  `/update`. Un état qu'on lit et qu'on écrit.
- **Commandes** — `/commands/*` (reboot, sirène, volet, flux, OTA). Un effet
  matériel déclenché : leur `200` signifie *accepté*, pas *effectué*.

S'y ajoutent le flux SSE `/api/v1/events`, les segments HLS `/stream/*`, et
les webhooks loopback `/events` + `/notifications` de l'ancien
`hl_event_collectd` (hors versionnage, en transition, cf. `docs/api.md`).
hlcamd remet désormais ses détections à openqiarad sur fbxbus, où il tient le
nom `hl_event_collectd` (`internal/fbxbus`).

Les routes non versionnées ont été supprimées : elles répondent `410 Gone`.

**Référence complète (payloads, codes d'erreur, contrat SSE) :
[`docs/api.md`](api.md).**

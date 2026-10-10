// Package hlevents traite les détections IntelliVision (humain/pet) que
// hlcamd remet à hl_event_collectd sur fbxbus, nom qu'openqiarad tient
// lui-même (cmd/openqiarad/event_collector.go). Le daemon vendor les
// mettait en file pour le cloud mort.
package hlevents

// NotificationItem est une notification horodatée.
type NotificationItem struct {
	Timestamp int64        `json:"ts"`
	Notif     Notification `json:"notif"`
}

// Notification décrit une notif. `Type` détermine quoi contient `Data`.
type Notification struct {
	Type string           `json:"type"`
	Data NotificationData `json:"data"`
}

// NotificationData pour les `iv_event` :
//
//	{"events":[{"eventType":"eObjectEnteredEvent","timestamp":...}],
//	 "objects":[{"class":"eOC_Human","confidence":1,"id":"26"}]}
//
// `events` peut être absent (certaines notifs n'ont que des objects).
// `objects` peut aussi être absent (events purs sans nouvel objet
// classifié, juste un Enter/Exit avec ID référencé d'une notif précédente).
type NotificationData struct {
	Events  []IVEvent  `json:"events,omitempty"`
	Objects []IVObject `json:"objects,omitempty"`
}

// IVEvent décrit une transition IntelliVision. EventType est l'un de :
//
//   - eObjectEnteredEvent : nouvel objet entré dans le champ
//   - eObjectExitedEvent  : objet sorti du champ
//   - eObjectClassified   : objet classifié (avec confidence dans le
//     objet correspondant via ID)
//   - eObjectLost         : objet perdu de vue (sans Exit propre)
type IVEvent struct {
	EventType string `json:"eventType"`
	Timestamp int64  `json:"timestamp"`
}

// IVObject décrit un objet détecté/classifié par IntelliVision.
//
//   - Class : eOC_Human ou eOC_Pet (autres classes inconnues)
//   - Confidence : float [0..1], 1 = certitude
//   - ID : identifiant interne IV (réutilisé entre Enter / Classify / Exit)
type IVObject struct {
	Class      string  `json:"class"`
	Confidence float64 `json:"confidence"`
	ID         string  `json:"id"`
}

// Classes IntelliVision connues (utiliser ces constantes pour les
// comparaisons typées plutôt que des string literals dispersées).
const (
	IVClassHuman = "eOC_Human"
	IVClassPet   = "eOC_Pet"
)

// EventTypes IntelliVision connus.
const (
	IVEventEntered    = "eObjectEnteredEvent"
	IVEventExited     = "eObjectExitedEvent"
	IVEventClassified = "eObjectClassified"
	IVEventLost       = "eObjectLost"
)

// NotifType valeurs connues au top-level d'une notification.
const (
	NotifTypeIV = "iv_event"
)

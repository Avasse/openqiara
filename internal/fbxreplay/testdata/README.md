# fbxhome radio transcripts

Anonymised excerpts of fbxhome's debug log (`/var/log/fbxhome.log` on the
camera). They are the oracle for replacing fbxhome: every frame fbxhome
received and sent, across several sensors, in steady state.

| File | Capture | What it covers |
|---|---|---|
| `steady_day.log` | production camera, ~10 h after a boot, fbxhome with the KPD→HlAlarm decoupling patch (Alarmo drives the alarm) | boot (siren get-state), one status heartbeat per sensor (read_status, then config), siren reboot with a full bytecode push, KPD PIN push, DWS/PIR events, siren keepalives every 10 min, MCU `UNREACHABLE` reports |
| `alarm_cycle.log` | production camera, ~5 min, fbxhome's own alarm logic driving the siren | KPD day arming, the siren commands fbxhome sends for arming and alert, siren reboot with a full bytecode push |

Radio addresses — `steady_day.log`: KPD 2, PIR 3, DWS 5, SRN 6.
`alarm_cycle.log`: DWS 3, PIR 4, KPD 6, SRN 7. The gateway is 1.

## Anonymisation

Applied before commit, nothing else was changed:

- only frame lines and a few context lines (`need …`, `manage a -> b`,
  `send config to node`, `mvt start/end`, siren state changes) are kept;
- the KPD PIN in the kpd-post frame is replaced by 0000 (`030004aaaa`);
- the data of every OpWrite in VM write frames (`wflags:13GM`) is zeroed:
  the sensor bytecode is proprietary, only opcodes, addresses and lengths
  remain;
- every timestamp — log lines, `55 01 <ts>` events and time frames
  (`wflags:8GM`) — is shifted by the same amount so that each file starts on
  a fake date; relative timing is preserved.

Before adding a transcript, check that `grep 'wflags:1, payload:0300'` (the
kpd-post frames fbxhome sends) only shows `030004aaaa`, and that no VM write
payload carries non-zero data.

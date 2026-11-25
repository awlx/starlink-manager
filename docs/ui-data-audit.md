# UI and data-exposure audit

The UI uses local templates, CSS and JavaScript. No runtime packages, CDNs,
API routes or authentication requirements were added or changed. `/clients`
is an HTML index (including an account-without-routers state); the existing
per-device clients API continues to reject dishes with HTTP 400. Metrics
remain on their separate private listener, never linked as a web-app page.

## Data inventory

| Fetched source / fields | Presentation and interpretation |
|---|---|
| Roster service-line number and nickname; terminal ID, kit/dish serial, location nickname, latitude/longitude, active/offline flags, last connected/disconnected | Device identity and expandable **Account roster details**. Coordinates are degrees; timestamps UTC. Missing flags/coordinates are not manufactured as false/zero. Roster offline is explicitly a snapshot, not current reachability. |
| Router roster ID, hardware version, bypass flag, service-line association | Router identity and roster details. Missing bypass flag is omitted. |
| Gateway `device_info`: ID, software/hardware versions, country code, UTC offset; `device_state.uptime_s` | Normalized diagnostics plus **Reported gateway fields**, which preserves every present schema field. UTC offset remains seconds; it does not shift chart timestamps. Gateway software and uptime take precedence over cloud fallback, including explicitly reported zero uptime. |
| Dish status: state, SNR, ping latency/drop, downlink/uplink, seconds to first nonempty slot, stow requested | Diagnostics/cards; uncommon fields also in gateway details. Latency is ms; drop fraction is rendered as percent in primary views; throughput is bits/s with decimal SI prefixes. Stow requested is a status flag, not a fetched history series. |
| Obstruction: fraction, currently obstructed, validity seconds, last-24h obstructed seconds, both wedge arrays | Badge/bar, diagnostics and gateway details. Fraction is sky-view obstruction, distinct from cloud obstruction time. Zero obstructed seconds is `0 s`, not unavailable; subsecond values are retained. Omitted fields are not shown as clear/zero. |
| All named gateway alert flags: motors stuck, thermal shutdown/throttle, mast not near vertical, unexpected location, slow Ethernet | Active warning labels on cards and diagnostics; raw true/false flags in gateway details. Cloud-only alerts also appear without a gateway snapshot. Ethernet 10/100 Mbps, no Ethernet link and unknown cloud names/codes remain visible. |
| History current counter; ping, drop, down/up, SNR, scheduled, obstructed arrays | Counter in diagnostics; seven charts. Scheduled/obstructed are stepped flags, not inferred maintenance events. Optional SNR/flag arrays preserve presence; unavailable historical values are not plotted as zero. Supplemental chart data is embedded in HTML, retaining the existing history API shape. |
| Derived history timestamps, downsampling | UTC tooltips; times are anchored from the device counter and poll time, not an independently reported device wall clock. Large windows use at most 2,000 averaged buckets; boolean buckets retain any true flag. Summaries therefore describe collected/bucketed samples, not a time-weighted outage percentage. |
| Cloud timestamp, software, down/up Mbps, ping drop average, ping latency average, obstruction time, signal quality, uptime, country, active alerts | Separate **Cloud telemetry** snapshot with its own UTC timestamp and explicit units, plus software/uptime fallback and merged alerts. Signal quality is 0–1, not SNR. Present zero differs from missing data. `ObstructionPercentTime` is labeled as an unconverted upstream value: its scale is not confirmed, so it is not misrepresented as sky-view obstruction. |
| Router gateway serial, SKU, WAN address, ping/drop, captive portal, uptime, embedded client list | Router diagnostics and reported gateway fields; no dish obstruction/SNR/throughput claims on router cards. |
| Client list name, MAC, IP, interface, signal; association time, mode, SNR, power-save mode, RX bytes/errors/streams, TX bytes/success bytes/streams | Primary client table plus **Full client diagnostics** from the last successful refresh. Ethernet/missing signal is not fabricated as 0 dBm. Optional low-level modes and SNR remain reported values without guessed meanings. |
| Account/session probe, cookies, tokens, probe bookkeeping | Account number and recoverable session-invalid banner only. Cookies/tokens and raw account/profile payloads are intentionally not exposed: the probe is required for authentication, not an inventory of personal account data. No additional fetch is needed or justified. |
| `get_diagnostics`, obstruction-map/context and other protocol messages not requested by the app | Not fetched; protocol definitions are not evidence of available cloud data. The cloud rejects `get_diagnostics`, so no failing diagnostic polling was introduced. Negotiated Ethernet speed is still not claimed; only upstream alert evidence is shown. |

New source metadata is persisted with the store. Older snapshots gain it on
their next successful poll; the UI explicitly says when field-preserving
metadata has not yet been collected. Polling, health reachability and metrics
semantics are unchanged. Retired devices remain visible with a neutral badge
and a 24-hour-history explanation, without being represented as healthy.

## Interaction fixes

1. **Stale warnings and mixed-time snapshots.** Server-rendered presentation refreshes every 30 seconds in stable regions:
  session/roster banners, complete diagnostics, obstruction bar and label,
  fleet counts and card snapshots update together. Failed refreshes preserve
  last-known state and show a persistent retry message. Changed inventory
  requests an explicit reload rather than moving keyboard focus.
2. **Chart keyboard/touch and empty-state gaps.** Charts retain focus and selected sample across resize/theme redraws; tap
  inspection remains visible. Empty charts are compact and explain that no
  history is not zero. Tooltips use explicit UTC timestamps.
3. **Ambiguous cancellation and missing busy feedback.** Power-save enable and disable have separate confirmations. Cancel never
  means disable; invalid minutes are rejected locally. In-flight commands
  show status text and disable other action buttons until completion.
4. **Overflow, navigation and error-page gaps.** Responsive wrapping covers nicknames, versions, raw field names and user
  navigation. Theme toggle text has normal foreground contrast; persisted
  choice survives navigation. Error pages have meaningful titles and H1s.
5. **Hidden and misrepresented source data.** The inventory above records the
   newly exposed roster, cloud, gateway and client diagnostics. Explicit
   presence distinguishes zero/false from missing data; retired devices and
   unknown units are identified instead of guessed.

## Verification

Live account observed: eight dishes, four unreachable, no routers. No real
device mutations were submitted; all real-device confirmations were dismissed.
Dashboard, online/offline detail, routerless clients, favicon and errors were
checked in a browser at 375, 768 and 2560 CSS pixels. Light/dark persistence,
touch inspection, keyboard chart focus/selection and Escape were exercised.
Session/roster failure and recovery used intercepted HTML fixtures, not a
deliberately invalidated live cookie. Router-specific and missing-data states
are covered by local fixtures because this account has no routers.

Focused regression coverage:

- `TestPresentationRoutesAndRouterlessAccount`
- `TestEmptyAndAllOfflineFleet`
- `TestCloudOnlyAndMissingGatewayFields`
- `TestRetiredDeviceAndSourceDetails`
- `TestRouterDiagnosticsDoNotShowDishFields`
- `TestReportedFieldsPreservePresenceAndNestedDiagnostics`
- `TestTelemetryAndRosterDistinguishMissingFromZero`
- `TestHistoryAndClientsKeepAdditionalReportedFields`

Deliberately unchanged: native browser confirmation dialogs; cloud-only
Ethernet link-speed limitations; the separate metrics listener; authentication
and admin gating; API top-level JSON contracts. Unconfirmed upstream scales
and numeric modes are shown honestly rather than assigned invented units.

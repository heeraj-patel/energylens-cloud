package householdenergy

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"time"

	"github.com/jackc/pgx/v5"
)

type retainedScope struct {
	hub, sensor, attachment string
	epoch                   int64
	start                   time.Time
}

type retainedRef struct {
	EventID  string `json:"event_id"`
	Revision int64  `json:"revision"`
}

type retainedHour struct {
	interval
	scope  retainedScope
	status string
	refs   []retainedRef
	stale  bool
}

func intervalScope(v interval, hour time.Time) retainedScope {
	return retainedScope{v.HubID, v.SensorID, v.AttachmentID, v.Epoch, hour.UTC()}
}

func refKey(scope retainedScope, ref retainedRef) string {
	return fmt.Sprintf("%s|%s|%s|%d|%s|%d", scope.hub, scope.sensor, scope.attachment, scope.epoch, ref.EventID, ref.Revision)
}

// A retained hour replaces all of its scoped raw slices as soon as any source
// observation has been purged. This prevents partial retention from adding the
// same event twice or dropping the purged share. A dirty, corrected, conflicting
// or boundary-cut hour is explicitly unavailable instead of being interpolated.
func mergeRetainedIntervals(ctx context.Context, tx pgx.Tx, home string, p period, raw []interval) ([]interval, error) {
	rows, err := tx.Query(ctx, `SELECT h.bucket_start,h.energy_wh::text,h.covered_seconds::text,h.status,h.source_refs,
 h.hub_id::text,h.ownership_epoch,h.sensor_id::text,h.attachment_id::text,
 a.appliance_id::text,s.kind,s.parent_sensor_id::text,
 topology.root_id::text,topology.root_kind,topology.root_id IS NOT NULL,NOT EXISTS (SELECT 1 FROM sensor_endpoints child WHERE child.home_id=s.home_id AND child.hub_id=s.hub_id AND child.ownership_epoch=s.ownership_epoch AND child.parent_sensor_id=s.id),
 EXISTS(SELECT 1 FROM metered_rollup_dirty d WHERE d.kind='energy_hour' AND d.home_id=h.home_id
   AND d.hub_id=h.hub_id AND d.ownership_epoch=h.ownership_epoch AND d.sensor_id=h.sensor_id
   AND d.attachment_id=h.attachment_id AND d.bucket_start=h.bucket_start)
 OR EXISTS(SELECT 1 FROM jsonb_to_recordset(h.source_refs) AS ref(event_id uuid,revision bigint)
   JOIN ingest_events e ON e.home_id=h.home_id AND e.hub_id=h.hub_id AND e.ownership_epoch=h.ownership_epoch
     AND e.event_id=ref.event_id AND e.revision>ref.revision) AS stale
 FROM energy_hourly h
 JOIN sensor_endpoints s ON s.id=h.sensor_id AND s.home_id=h.home_id AND s.hub_id=h.hub_id AND s.ownership_epoch=h.ownership_epoch
 JOIN attachments a ON a.id=h.attachment_id AND a.home_id=h.home_id AND a.sensor_id=h.sensor_id
 LEFT JOIN LATERAL (WITH RECURSIVE ancestry(id,parent_sensor_id,kind,depth) AS (
   SELECT s.id,s.parent_sensor_id,s.kind,0
   UNION ALL
   SELECT parent.id,parent.parent_sensor_id,parent.kind,ancestry.depth+1
   FROM ancestry JOIN sensor_endpoints parent ON parent.id=ancestry.parent_sensor_id AND parent.home_id=s.home_id AND parent.hub_id=s.hub_id AND parent.ownership_epoch=s.ownership_epoch
   WHERE ancestry.depth < 32
 ) SELECT id AS root_id,kind AS root_kind FROM ancestry WHERE parent_sensor_id IS NULL ORDER BY depth DESC LIMIT 1) topology ON TRUE
 WHERE h.home_id=$1 AND h.bucket_start<$3 AND h.bucket_start+interval '1 hour'>$2
 ORDER BY h.bucket_start,h.hub_id,h.ownership_epoch,h.sensor_id,h.attachment_id`, home, p.Start, p.End)
	if err != nil {
		return nil, err
	}
	hours := []retainedHour{}
	for rows.Next() {
		var h retainedHour
		var value *string
		var covered *string
		var refs []byte
		if err = rows.Scan(&h.Start, &value, &covered, &h.status, &refs, &h.HubID, &h.Epoch, &h.SensorID, &h.AttachmentID, &h.ApplianceID, &h.SensorKind, &h.ParentSensorID, &h.TopologyRootID, &h.TopologyRootKind, &h.TopologyComplete, &h.TopologyTerminal, &h.stale); err != nil {
			rows.Close()
			return nil, err
		}
		h.End = h.Start.Add(time.Hour)
		fuel := "electricity" // Energy interval ingestion accepts this fuel only.
		h.Fuel = &fuel
		h.scope = intervalScope(h.interval, h.Start)
		if err = json.Unmarshal(refs, &h.refs); err != nil {
			rows.Close()
			return nil, err
		}
		if value != nil {
			var ok bool
			h.Wh, ok = new(big.Rat).SetString(*value)
			if !ok {
				rows.Close()
				return nil, fmt.Errorf("invalid retained energy decimal")
			}
		}
		if covered != nil {
			var ok bool
			h.Covered, ok = new(big.Rat).SetString(*covered)
			if !ok {
				rows.Close()
				return nil, fmt.Errorf("invalid retained coverage decimal")
			}
		}
		hours = append(hours, h)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}

	rawRefs := make(map[string]bool)
	for _, v := range raw {
		for hour := v.Start.UTC().Truncate(time.Hour); hour.Before(v.End); hour = hour.Add(time.Hour) {
			rawRefs[refKey(intervalScope(v, hour), retainedRef{v.EventID, v.Revision})] = true
		}
	}
	selected := make(map[retainedScope]bool)
	result := make([]interval, 0, len(raw)+len(hours))
	for _, h := range hours {
		if h.status == "no_intervals" && len(h.refs) == 0 {
			continue
		}
		missing := len(h.refs) == 0
		for _, ref := range h.refs {
			if !rawRefs[refKey(h.scope, ref)] {
				missing = true
			}
		}
		if !missing {
			continue
		}
		selected[h.scope] = true
		h.Summarized = true
		if h.stale || h.status != "complete" || h.Wh == nil || h.Start.Before(p.Start) || h.End.After(p.End) {
			h.Unavailable = true
			h.Wh = nil
			h.Covered = nil
		}
		result = append(result, h.interval)
	}
	for _, v := range raw {
		start := v.Start
		for start.Before(v.End) {
			hour := start.UTC().Truncate(time.Hour)
			end := minTime(v.End, hour.Add(time.Hour))
			if !selected[intervalScope(v, hour)] {
				part := v
				part.Wh = new(big.Rat).Mul(v.Wh, new(big.Rat).SetFrac(big.NewInt(end.Sub(start).Nanoseconds()), big.NewInt(v.End.Sub(v.Start).Nanoseconds())))
				if v.Covered != nil {
					part.Covered = new(big.Rat).Mul(v.Covered, new(big.Rat).SetFrac(big.NewInt(end.Sub(start).Nanoseconds()), big.NewInt(v.End.Sub(v.Start).Nanoseconds())))
				}
				part.Start, part.End = start, end
				result = append(result, part)
			}
			start = end
		}
	}
	return result, nil
}

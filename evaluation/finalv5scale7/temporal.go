package finalv5scale7

import (
	"time"
)

var epoch20200101 = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
var noon20200101 = time.Date(2020, 1, 1, 12, 0, 0, 0, time.UTC)

var datasetRegions = [5]string{"north", "south", "east", "west", "central"}

// RowRegion is (ARRAY['north','south','east','west','central'])[((row_id-1) % 5) + 1].
func RowRegion(rowID int64) string { return datasetRegions[(rowID-1)%5] }

// AddDays20200101 is date '2020-01-01' + N days in canonical d: form.
func AddDays20200101(days int64) string {
	return epoch20200101.AddDate(0, 0, int(days)).Format("2006-01-02")
}

// Timestamp20200101 is timestamp '2020-01-01 00:00:00' + seconds + micros in
// the production ts: canonical form (six fractional digits when nonzero).
func Timestamp20200101(seconds, micros int64) string {
	value := epoch20200101.Add(time.Duration(seconds)*time.Second + time.Duration(micros)*time.Microsecond)
	return canonicalTimestampNoTZ(value)
}

// TimestampNoon20200101 is timestamp '2020-01-01 12:00:00' + N seconds.
func TimestampNoon20200101(seconds int64) string {
	value := noon20200101.Add(time.Duration(seconds) * time.Second)
	return canonicalTimestampNoTZ(value)
}

// canonicalTimestampNoTZ mirrors internal/exposure canonicalTimestamp output
// for timestamp-without-time-zone values: microsecond truncation and Go's
// trailing-zero-trimming fractional format, exactly as production emits it.
func canonicalTimestampNoTZ(value time.Time) string {
	return value.Truncate(time.Microsecond).Format("2006-01-02T15:04:05.999999")
}

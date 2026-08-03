package finalv5oracle

import (
	"errors"
	"fmt"
	"strconv"
	"time"
)

const (
	ArtifactGeneratorVersion = "taskgate-final-v5-result-heavy-generator-v1"
	ArtifactGeneratorSeed    = int64(20260803)
)

var artifactColumnsV1 = [...]ResultColumn{
	{Name: "row_id", Type: SQLBigInt},
	{Name: "category", Type: SQLText},
	{Name: "amount", Type: SQLNumeric},
	{Name: "event_date", Type: SQLDate},
	{Name: "sequence_no", Type: SQLInteger},
	{Name: "approved", Type: SQLBoolean},
	{Name: "event_timestamp", Type: SQLTimestampWithoutTZ},
	{Name: "description", Type: SQLText},
	{Name: "quantity", Type: SQLBigInt},
	{Name: "unit_price", Type: SQLNumeric},
	{Name: "tax_amount", Type: SQLNumeric},
	{Name: "settled_date", Type: SQLDate},
	{Name: "processed_at", Type: SQLTimestampWithoutTZ},
	{Name: "region", Type: SQLText},
	{Name: "revision", Type: SQLInteger},
	{Name: "active", Type: SQLBoolean},
}

// ArtifactSchema returns the reviewed x4 or x16 schema. The x4 schema is the
// exact leading projection of x16.
func ArtifactSchema(columnCount int) ([]ResultColumn, error) {
	if columnCount != 4 && columnCount != 16 {
		return nil, errors.New("artifact column count must be 4 or 16")
	}
	return append([]ResultColumn(nil), artifactColumnsV1[:columnCount]...), nil
}

func validateArtifactShape(rowCount int64, columnCount int) error {
	if rowCount != 100 && rowCount != 10_000 && rowCount != 100_000 {
		return errors.New("artifact row count must be 100, 10000, or 100000")
	}
	if columnCount != 4 && columnCount != 16 {
		return errors.New("artifact column count must be 4 or 16")
	}
	return nil
}

// ArtifactRow deterministically constructs one zero-based result-heavy row.
// The first four values never depend on the requested width.
func ArtifactRow(rowIndex int64, columnCount int) ([]any, error) {
	if rowIndex < 0 || rowIndex >= 100_000 {
		return nil, errors.New("artifact row index is outside the formal maximum")
	}
	if columnCount != 4 && columnCount != 16 {
		return nil, errors.New("artifact column count must be 4 or 16")
	}
	rowNumber := rowIndex + 1
	baseDate := time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC)
	eventDate := baseDate.AddDate(0, 0, int(rowIndex%3_653)).Format("2006-01-02")
	row := make([]any, columnCount)
	row[0] = rowNumber
	row[1] = []string{"alpha", "beta", "gamma", "delta"}[rowIndex%4]
	row[2] = artifactDecimal((rowNumber*7_919)%100_000_000, 2)
	row[3] = eventDate
	if columnCount == 4 {
		return row, nil
	}
	row[4] = int32(rowNumber % 1_000_000)
	row[5] = rowNumber%3 != 0
	row[6] = baseDate.Add(time.Duration(rowIndex)*time.Second + time.Duration(rowIndex%1_000)*time.Microsecond)
	row[7] = "artifact-row-" + strconv.FormatInt(rowNumber, 10)
	row[8] = int64(1 + rowIndex%10_000)
	row[9] = artifactDecimal((rowNumber*104_729)%10_000_000, 4)
	tax := (rowNumber * 37) % 1_000_000
	if rowNumber%11 == 0 {
		tax = -tax
	}
	row[10] = artifactDecimal(tax, 2)
	row[11] = baseDate.AddDate(0, 0, int((rowIndex+31)%3_653)).Format("2006-01-02")
	row[12] = baseDate.Add(12*time.Hour + time.Duration(rowIndex)*time.Minute)
	row[13] = []string{"north", "south", "east", "west", "central"}[rowIndex%5]
	row[14] = int32(rowIndex % 97)
	row[15] = rowNumber%7 != 0
	return row, nil
}

// StreamArtifactRows emits an exact formal shape without retaining prior
// rows. The callback may retain its row because every emission is detached.
func StreamArtifactRows(rowCount int64, columnCount int, yield func([]any) error) error {
	if err := validateArtifactShape(rowCount, columnCount); err != nil {
		return err
	}
	if yield == nil {
		return errors.New("artifact row callback is nil")
	}
	for index := int64(0); index < rowCount; index++ {
		row, err := ArtifactRow(index, columnCount)
		if err != nil {
			return err
		}
		if err := yield(row); err != nil {
			return fmt.Errorf("artifact row %d: %w", index+1, err)
		}
	}
	return nil
}

// ArtifactResultSummary independently computes the expected logical result
// for one of the six formal Artifact cells.
func ArtifactResultSummary(rowCount int64, columnCount int) (ResultSummary, error) {
	columns, err := ArtifactSchema(columnCount)
	if err != nil {
		return ResultSummary{}, err
	}
	hasher, err := NewResultHasher(columns)
	if err != nil {
		return ResultSummary{}, err
	}
	if err := StreamArtifactRows(rowCount, columnCount, hasher.WriteRow); err != nil {
		return ResultSummary{}, err
	}
	return hasher.Finalize()
}

func artifactDecimal(scaled int64, scale int) string {
	negative := scaled < 0
	if negative {
		scaled = -scaled
	}
	power := int64(1)
	for index := 0; index < scale; index++ {
		power *= 10
	}
	value := strconv.FormatInt(scaled/power, 10)
	if scale > 0 {
		value += "." + fmt.Sprintf("%0*d", scale, scaled%power)
	}
	if negative && scaled != 0 {
		value = "-" + value
	}
	return value
}

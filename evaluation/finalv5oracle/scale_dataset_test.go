package finalv5oracle

import "testing"

func TestExposureScaleTypedDatasetStreamAgreesWithFormula(t *testing.T) {
	product, err := exposureScaleDatasetProduct()
	if err != nil {
		t.Fatal(err)
	}
	agreement, err := AgreeExposureScaleDatasetStream(ExposureScaleDatasetStreamColumns(), func(yield func([]any) error) error {
		for index := int64(0); index < product.rowCount; index++ {
			row, err := product.row(index)
			if err != nil {
				return err
			}
			if err := yield(row); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !agreement.Agreed || agreement.Reference != agreement.Observed ||
		agreement.Reference.RowCount != 414_000 ||
		agreement.Reference.SHA256 != "8ada6ddeeb221e24b906493734fa613c1e222c15864f29ed6398b4b8f1bb34f6" {
		t.Fatalf("typed exposure-scale agreement = %+v", agreement)
	}
	if len(ExposureScaleDatasetProductSpec().Fields) != 4 {
		t.Fatal("detached exposure-scale Product schema is not four typed fields")
	}
}

func TestExposureScaleTypedDatasetStreamRejectsSchemaAndRows(t *testing.T) {
	columns := ExposureScaleDatasetStreamColumns()
	columns[0], columns[1] = columns[1], columns[0]
	if _, err := AgreeExposureScaleDatasetStream(columns, func(func([]any) error) error { return nil }); err == nil {
		t.Fatal("reordered PostgreSQL field descriptions were accepted")
	}

	product, err := exposureScaleDatasetProduct()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AgreeExposureScaleDatasetStream(ExposureScaleDatasetStreamColumns(), func(yield func([]any) error) error {
		for index := int64(0); index < product.rowCount-1; index++ {
			row, err := product.row(index)
			if err != nil {
				return err
			}
			if err := yield(row); err != nil {
				return err
			}
		}
		return nil
	}); err == nil {
		t.Fatal("truncated 413,999-row PostgreSQL stream was accepted")
	}
}

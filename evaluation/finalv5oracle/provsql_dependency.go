package finalv5oracle

import (
	"bufio"
	"bytes"
	"container/heap"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strconv"
)

const (
	ProvSQLDependencyGeneratorVersion = "taskgate-final-v5-provsql-dependency-v1"
	ProvSQLFactsPerOrder              = int64(29)
	ProvSQLNonceFacts                 = int64(3)
)

type ProvSQLDependencyGenerationStats struct {
	FactEmissions       int64 `json:"fact_emissions"`
	PeakBufferedMembers int   `json:"peak_buffered_members"`
	SpillRuns           int   `json:"spill_runs"`
	PeakMergeHeads      int   `json:"peak_merge_heads"`
}

// ProvSQLDependencyOracleReport is the independently regenerated expectation
// for one frozen scale/nonce candidate. Candidate contains base-row plus every
// typed base-cell Fact from L orders, 5L lineitems, and one nonce row.
type ProvSQLDependencyOracleReport struct {
	GeneratorVersion string                           `json:"generator_version"`
	Scale            string                           `json:"scale"`
	Limit            int64                            `json:"limit"`
	Nonce            int64                            `json:"nonce"`
	Candidate        StreamSetSummary                 `json:"candidate"`
	Result           ResultSummary                    `json:"result"`
	Release          StreamSetSummary                 `json:"release"`
	Stats            ProvSQLDependencyGenerationStats `json:"stats"`
}

// GenerateProvSQLNonceJoinDependency constructs candidate, logical result, and
// release expectations from the frozen typed Product formulas. It never accepts
// SQL or consumes a production response, Sample, evidence, or another oracle.
func GenerateProvSQLNonceJoinDependency(cell ProvSQLNonceJoinCell, options StreamSetOptions) (ProvSQLDependencyOracleReport, error) {
	if err := cell.validate(); err != nil {
		return ProvSQLDependencyOracleReport{}, err
	}
	wantFacts := ProvSQLFactsPerOrder*cell.Limit + ProvSQLNonceFacts
	candidate, err := SummarizeSemanticSet("candidate", func(yield func(string) error) error {
		return StreamProvSQLNonceJoinFacts(cell.Limit, cell.Nonce, func(fact CanonicalFact) error {
			return yield(fact.SHA256)
		})
	}, options)
	if err != nil {
		return ProvSQLDependencyOracleReport{}, fmt.Errorf("summarize ProvSQL candidate FactSet: %w", err)
	}
	if candidate.Cardinality != wantFacts || candidate.Stats.DuplicateMembers != 0 {
		return ProvSQLDependencyOracleReport{}, fmt.Errorf("ProvSQL candidate cardinality = %d, want 29L+3 = %d", candidate.Cardinality, wantFacts)
	}

	rows, aggregates, err := provSQLLogicalRows(cell.Limit)
	if err != nil {
		return ProvSQLDependencyOracleReport{}, err
	}
	result, err := CanonicalResult(provSQLResultColumns(), rows)
	if err != nil {
		return ProvSQLDependencyOracleReport{}, fmt.Errorf("canonicalize ProvSQL logical result: %w", err)
	}
	releaseFacts := make([]CanonicalFact, 0, 12)
	for status := int64(0); status < 3; status++ {
		facts, buildErr := buildProvSQLReleaseFacts(cell.Limit, cell.Nonce, status, aggregates[status], options)
		if buildErr != nil {
			return ProvSQLDependencyOracleReport{}, fmt.Errorf("construct ProvSQL release group %d: %w", status, buildErr)
		}
		releaseFacts = append(releaseFacts, facts[:]...)
	}
	release, err := SummarizeSemanticSet("candidate", func(yield func(string) error) error {
		for _, fact := range releaseFacts {
			if err := yield(fact.SHA256); err != nil {
				return err
			}
		}
		return nil
	}, options)
	if err != nil {
		return ProvSQLDependencyOracleReport{}, fmt.Errorf("summarize ProvSQL release FactSet: %w", err)
	}
	if release.Cardinality != 12 || release.Stats.DuplicateMembers != 0 || result.RowCount != 3 || result.ColumnCount != 4 {
		return ProvSQLDependencyOracleReport{}, errors.New("ProvSQL result/release generator violated the fixed 3x4 shape")
	}
	return ProvSQLDependencyOracleReport{
		GeneratorVersion: ProvSQLDependencyGeneratorVersion,
		Scale:            cell.Scale, Limit: cell.Limit, Nonce: cell.Nonce,
		Candidate: candidate, Result: result, Release: release,
		Stats: ProvSQLDependencyGenerationStats{
			FactEmissions: wantFacts, PeakBufferedMembers: candidate.Stats.PeakBufferedMembers,
			SpillRuns: candidate.Stats.SpillRuns, PeakMergeHeads: candidate.Stats.PeakMergeHeads,
		},
	}, nil
}

// StreamProvSQLNonceJoinFacts emits exactly 29L+3 distinct base Facts. Rows are
// formula-generated in stable-key order and only one typed row is buffered.
func StreamProvSQLNonceJoinFacts(limit, nonce int64, yield func(CanonicalFact) error) error {
	if !provSQLFormalLimit(limit) || nonce < 1 || nonce > 1_000 || yield == nil {
		return errors.New("ProvSQL dependency stream requires a formal limit, frozen nonce, and callback")
	}
	products, err := provSQLDatasetProducts()
	if err != nil {
		return err
	}
	for _, selected := range []struct {
		product benchmarkDatasetProduct
		first   int64
		last    int64
	}{
		{product: products[0], first: 0, last: limit},
		{product: products[1], first: 0, last: 5 * limit},
		{product: products[2], first: nonce - 1, last: nonce},
	} {
		for rowIndex := selected.first; rowIndex < selected.last; rowIndex++ {
			facts, factErr := buildProvSQLProductRowFacts(selected.product, rowIndex)
			if factErr != nil {
				return factErr
			}
			for _, fact := range facts {
				if err := yield(fact); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func buildProvSQLProductRowFacts(product benchmarkDatasetProduct, rowIndex int64) ([]CanonicalFact, error) {
	values, entityKey, err := provSQLProductRowIdentity(product, rowIndex)
	if err != nil {
		return nil, err
	}
	rowFact, err := BuildV2BaseRowFact(V2BaseRowInput{
		SourceNamespace: product.sourceNamespace, Snapshot: product.snapshot, EntityKey: entityKey,
	})
	if err != nil {
		return nil, err
	}
	result := make([]CanonicalFact, 1, len(product.fields)+1)
	result[0] = rowFact
	for index, field := range product.fields {
		canonical, err := provSQLCanonicalFactValue(field.SQLType, values[index])
		if err != nil {
			return nil, fmt.Errorf("ProvSQL Product %s field %s: %w", product.productID, field.Name, err)
		}
		fact, err := BuildV2BaseCellFact(V2BaseCellInput{
			SourceNamespace: product.sourceNamespace, Snapshot: product.snapshot, EntityKey: entityKey,
			// Multi-Product ordinal publications freeze role-qualified canonical
			// field IDs; this is not the visible SQL alias and must not be bare.
			Field: product.productID + "." + field.Name, SQLType: string(field.SQLType), CanonicalValue: canonical,
		})
		if err != nil {
			return nil, err
		}
		result = append(result, fact)
	}
	return result, nil
}

func provSQLProductRowIdentity(product benchmarkDatasetProduct, rowIndex int64) ([]any, string, error) {
	values, err := product.row(rowIndex)
	if err != nil {
		return nil, "", err
	}
	components := []string{product.sourceNamespace}
	for index, field := range product.fields {
		if !field.StableKey {
			continue
		}
		canonical, err := provSQLCanonicalFactValue(field.SQLType, values[index])
		if err != nil {
			return nil, "", err
		}
		components = append(components, field.Name, string(field.SQLType), canonical)
	}
	entityKey, err := ComposeOracleCanonicalKeyV2("base-entity", components...)
	if err != nil {
		return nil, "", err
	}
	return values, entityKey, nil
}

func provSQLCanonicalFactValue(sqlType SQLType, raw any) (string, error) {
	typed, err := NormalizeTypedValue(sqlType, raw)
	if err != nil {
		return "", err
	}
	if typed.IsNull() {
		return "", errors.New("frozen ProvSQL Product contains NULL")
	}
	switch sqlType {
	case SQLBigInt:
		value, err := canonicalSignedInteger(raw, 64)
		if err != nil {
			return "", err
		}
		return "i:" + strconv.FormatInt(value, 10), nil
	case SQLInteger:
		value, err := canonicalSignedInteger(raw, 32)
		if err != nil {
			return "", err
		}
		return "i:" + strconv.FormatInt(value, 10), nil
	case SQLNumeric:
		rational, ok := new(big.Rat).SetString(string(typed.CanonicalBytes()))
		if !ok {
			return "", errors.New("frozen ProvSQL numeric is not an exact rational")
		}
		return "n:" + rational.RatString(), nil
	default:
		return "", fmt.Errorf("ProvSQL Fact type %q is unsupported", sqlType)
	}
}

type provSQLAggregate struct {
	cents   int64
	lines   int64
	members int64
}

func provSQLResultColumns() []ResultColumn {
	return []ResultColumn{{Name: "status", Type: SQLBigInt}, {Name: "price", Type: SQLText},
		{Name: "lines", Type: SQLBigInt}, {Name: "members", Type: SQLBigInt}}
}

// ProvSQLResultSchema returns the fixed four-column logical result contract
// shared by the independent oracle and publication binding live comparison.
func ProvSQLResultSchema() []ResultColumn {
	return append([]ResultColumn(nil), provSQLResultColumns()...)
}

func provSQLLogicalRows(limit int64) ([][]any, [3]provSQLAggregate, error) {
	if !provSQLFormalLimit(limit) {
		return nil, [3]provSQLAggregate{}, errors.New("ProvSQL logical result requires a formal limit")
	}
	groups := [3]provSQLAggregate{}
	for orderKey := int64(1); orderKey <= limit; orderKey++ {
		status := orderKey % 3
		for line := int64(1); line <= 5; line++ {
			one := groups[status]
			one.cents += ((orderKey*13)+(line*7))%100_000 + 100
			one.lines += line
			one.members++
			groups[status] = one
		}
	}
	rows := make([][]any, 3)
	for status := int64(0); status < 3; status++ {
		one := groups[status]
		rows[status] = []any{status, fmt.Sprintf("%d.%02d", one.cents/100, one.cents%100), one.lines, one.members}
	}
	return rows, groups, nil
}

func buildProvSQLReleaseFacts(limit, nonce, status int64, aggregate provSQLAggregate,
	options StreamSetOptions) ([4]CanonicalFact, error) {
	if status < 0 || status > 2 {
		return [4]CanonicalFact{}, errors.New("ProvSQL release status is outside 0..2")
	}
	products, err := provSQLDatasetProducts()
	if err != nil {
		return [4]CanonicalFact{}, err
	}
	orders, lineitem, nonceProduct := products[0], products[1], products[2]
	statusWitness, err := summarizeProvSQLWeightedWitness(func(yield func(string, uint64) error) error {
		for orderKey := status; orderKey <= limit; orderKey += 3 {
			if orderKey == 0 {
				continue
			}
			facts, err := buildProvSQLProductRowFacts(orders, orderKey-1)
			if err != nil {
				return err
			}
			if err := yield(facts[2].SHA256, 5); err != nil { // provsql_orders.status
				return err
			}
		}
		return nil
	}, options)
	if err != nil {
		return [4]CanonicalFact{}, err
	}
	priceWitness, err := summarizeProvSQLWeightedWitness(func(yield func(string, uint64) error) error {
		return streamProvSQLGroupLineitemFact(lineitem, limit, status, 3, yield) // extendedprice
	}, options)
	if err != nil {
		return [4]CanonicalFact{}, err
	}
	linesWitness, err := summarizeProvSQLWeightedWitness(func(yield func(string, uint64) error) error {
		return streamProvSQLGroupLineitemFact(lineitem, limit, status, 2, yield) // linenumber
	}, options)
	if err != nil {
		return [4]CanonicalFact{}, err
	}
	countWitness, err := summarizeProvSQLWeightedWitness(func(yield func(string, uint64) error) error {
		for orderKey := status; orderKey <= limit; orderKey += 3 {
			if orderKey == 0 {
				continue
			}
			facts, err := buildProvSQLProductRowFacts(orders, orderKey-1)
			if err != nil {
				return err
			}
			// Each order participates in five joined members. orderkey is one
			// leaf predicate plus one join key; partition_key is one scope
			// predicate plus two join keys.
			for _, member := range []struct {
				hash string
				mult uint64
			}{{facts[0].SHA256, 5}, {facts[1].SHA256, 10}, {facts[3].SHA256, 15}} {
				if err := yield(member.hash, member.mult); err != nil {
					return err
				}
			}
			for line := int64(1); line <= 5; line++ {
				lineIndex := (orderKey-1)*5 + (line - 1)
				lineFacts, err := buildProvSQLProductRowFacts(lineitem, lineIndex)
				if err != nil {
					return err
				}
				for _, member := range []struct {
					hash string
					mult uint64
				}{{lineFacts[0].SHA256, 1}, {lineFacts[1].SHA256, 1}, {lineFacts[4].SHA256, 2}} {
					if err := yield(member.hash, member.mult); err != nil {
						return err
					}
				}
			}
		}
		nonceFacts, err := buildProvSQLProductRowFacts(nonceProduct, nonce-1)
		if err != nil {
			return err
		}
		members := uint64(aggregate.members)
		for _, member := range []struct {
			hash string
			mult uint64
		}{{nonceFacts[0].SHA256, members}, {nonceFacts[1].SHA256, members}, {nonceFacts[2].SHA256, 2 * members}} {
			if err := yield(member.hash, member.mult); err != nil {
				return err
			}
		}
		return nil
	}, options)
	if err != nil {
		return [4]CanonicalFact{}, err
	}

	statusExpression := orders.sourceNamespace + "." + orders.productID + ".status"
	priceExpression := lineitem.sourceNamespace + "." + lineitem.productID + ".extendedprice"
	linesExpression := lineitem.sourceNamespace + "." + lineitem.productID + ".linenumber"
	groupKey, err := ComposeOracleCanonicalKeyV2("group-row", statusExpression+"\x00bigint\x00i:"+strconv.FormatInt(status, 10))
	if err != nil {
		return [4]CanonicalFact{}, err
	}
	bundle := []V2SnapshotBinding{
		{SourceNamespace: orders.sourceNamespace, Snapshot: orders.snapshot},
		{SourceNamespace: lineitem.sourceNamespace, Snapshot: lineitem.snapshot},
		{SourceNamespace: nonceProduct.sourceNamespace, Snapshot: nonceProduct.snapshot},
	}
	inputs := [4]V2DerivedInput{
		{SnapshotBundle: bundle, OutputRowKey: groupKey, NormalizedExpression: "group(" + statusExpression + ")",
			SQLType: "bigint", CanonicalValue: "i:" + strconv.FormatInt(status, 10), WitnessCommitment: statusWitness},
		{SnapshotBundle: bundle, OutputRowKey: groupKey, NormalizedExpression: "sum(" + priceExpression + ")",
			SQLType: "numeric", CanonicalValue: "n:" + new(big.Rat).SetFrac64(aggregate.cents, 100).RatString(), WitnessCommitment: priceWitness},
		{SnapshotBundle: bundle, OutputRowKey: groupKey, NormalizedExpression: "sum(" + linesExpression + ")",
			SQLType: "bigint", CanonicalValue: "i:" + strconv.FormatInt(aggregate.lines, 10), WitnessCommitment: linesWitness},
		{SnapshotBundle: bundle, OutputRowKey: groupKey, NormalizedExpression: "count(*)",
			SQLType: "bigint", CanonicalValue: "i:" + strconv.FormatInt(aggregate.members, 10), WitnessCommitment: countWitness},
	}
	var result [4]CanonicalFact
	for index, input := range inputs {
		result[index], err = BuildV2DerivedFact(input)
		if err != nil {
			return [4]CanonicalFact{}, err
		}
	}
	return result, nil
}

func streamProvSQLGroupLineitemFact(product benchmarkDatasetProduct, limit, status int64, factIndex int,
	yield func(string, uint64) error) error {
	for orderKey := status; orderKey <= limit; orderKey += 3 {
		if orderKey == 0 {
			continue
		}
		for line := int64(1); line <= 5; line++ {
			rowIndex := (orderKey-1)*5 + line - 1
			facts, err := buildProvSQLProductRowFacts(product, rowIndex)
			if err != nil {
				return err
			}
			if err := yield(facts[factIndex].SHA256, 1); err != nil {
				return err
			}
		}
	}
	return nil
}

type provSQLWeightedMember struct {
	hash         [sha256.Size]byte
	multiplicity uint64
}

type provSQLWeightedStream func(func(string, uint64) error) error

// summarizeProvSQLWeightedWitness is a bounded external sorter for the public
// V2 witness-multiset encoding. It is not SQL preparation or Fact derivation;
// callers supply already independently constructed base Fact hashes.
func summarizeProvSQLWeightedWitness(stream provSQLWeightedStream, options StreamSetOptions) (string, error) {
	if stream == nil {
		return "", errors.New("ProvSQL weighted witness stream is nil")
	}
	limit := options.MaxInMemoryMembers
	if limit == 0 {
		limit = defaultStreamSetBufferMembers
	}
	if limit < 2 {
		return "", errors.New("ProvSQL weighted witness memory bound must be at least two")
	}
	buffer := make([]provSQLWeightedMember, 0, limit)
	var directory string
	var paths []string
	defer func() {
		if directory != "" {
			_ = os.RemoveAll(directory)
		}
	}()
	flush := func() error {
		if len(buffer) == 0 {
			return nil
		}
		if directory == "" {
			var err error
			directory, err = os.MkdirTemp(options.TempDir, "taskgate-final-v5-provsql-witness-")
			if err != nil {
				return err
			}
		}
		sort.Slice(buffer, func(i, j int) bool { return bytes.Compare(buffer[i].hash[:], buffer[j].hash[:]) < 0 })
		path := filepath.Join(directory, fmt.Sprintf("run-%06d.bin", len(paths)))
		file, err := os.Create(path)
		if err != nil {
			return err
		}
		writer := bufio.NewWriterSize(file, 128*1024)
		for index, member := range buffer {
			if index > 0 && member.hash == buffer[index-1].hash {
				_ = file.Close()
				return errors.New("ProvSQL weighted witness emitted a duplicate Fact")
			}
			if err := writeProvSQLWeightedMember(writer, member); err != nil {
				_ = file.Close()
				return err
			}
		}
		if err := writer.Flush(); err != nil {
			_ = file.Close()
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
		paths = append(paths, path)
		buffer = buffer[:0]
		return nil
	}
	count := uint64(0)
	err := stream(func(hashText string, multiplicity uint64) error {
		decoded, err := hex.DecodeString(hashText)
		if err != nil || len(decoded) != sha256.Size || multiplicity == 0 {
			return errors.New("ProvSQL weighted witness member is invalid")
		}
		if len(buffer) == limit {
			if err := flush(); err != nil {
				return err
			}
		}
		var member provSQLWeightedMember
		copy(member.hash[:], decoded)
		member.multiplicity = multiplicity
		buffer = append(buffer, member)
		count++
		return nil
	})
	if err != nil {
		return "", err
	}
	if count == 0 || count > ^uint64(0)/2 {
		return "", errors.New("ProvSQL weighted witness is empty or too large")
	}
	target := sha256.New()
	oracleWriteHashString(target, "witness-multiset")
	writeUint64(target, count*2)
	writeMember := func(member provSQLWeightedMember) error {
		oracleWriteHashString(target, hex.EncodeToString(member.hash[:]))
		oracleWriteHashString(target, fmt.Sprintf("%020d", member.multiplicity))
		return nil
	}
	if len(paths) == 0 {
		sort.Slice(buffer, func(i, j int) bool { return bytes.Compare(buffer[i].hash[:], buffer[j].hash[:]) < 0 })
		for index, member := range buffer {
			if index > 0 && member.hash == buffer[index-1].hash {
				return "", errors.New("ProvSQL weighted witness emitted a duplicate Fact")
			}
			_ = writeMember(member)
		}
		return hex.EncodeToString(target.Sum(nil)), nil
	}
	if err := flush(); err != nil {
		return "", err
	}
	readers := make([]provSQLWeightedRun, len(paths))
	for index, path := range paths {
		file, err := os.Open(path)
		if err != nil {
			closeProvSQLWeightedRuns(readers)
			return "", err
		}
		readers[index] = provSQLWeightedRun{file: file, reader: bufio.NewReaderSize(file, 128*1024)}
	}
	defer closeProvSQLWeightedRuns(readers)
	heads := &provSQLWeightedHeap{}
	heap.Init(heads)
	for index := range readers {
		member, found, err := readProvSQLWeightedMember(readers[index].reader)
		if err != nil {
			return "", err
		}
		if found {
			heap.Push(heads, provSQLWeightedHead{member: member, run: index})
		}
	}
	var previous [sha256.Size]byte
	havePrevious := false
	var written uint64
	for heads.Len() > 0 {
		head := heap.Pop(heads).(provSQLWeightedHead)
		if havePrevious && head.member.hash == previous {
			return "", errors.New("ProvSQL weighted witness emitted a cross-run duplicate Fact")
		}
		_ = writeMember(head.member)
		previous, havePrevious = head.member.hash, true
		written++
		next, found, err := readProvSQLWeightedMember(readers[head.run].reader)
		if err != nil {
			return "", err
		}
		if found {
			heap.Push(heads, provSQLWeightedHead{member: next, run: head.run})
		}
	}
	if written != count {
		return "", errors.New("ProvSQL weighted witness merge lost a member")
	}
	return hex.EncodeToString(target.Sum(nil)), nil
}

func writeProvSQLWeightedMember(writer io.Writer, member provSQLWeightedMember) error {
	var multiplicity [8]byte
	binary.BigEndian.PutUint64(multiplicity[:], member.multiplicity)
	if _, err := writer.Write(member.hash[:]); err != nil {
		return err
	}
	_, err := writer.Write(multiplicity[:])
	return err
}

func readProvSQLWeightedMember(reader io.Reader) (provSQLWeightedMember, bool, error) {
	var encoded [sha256.Size + 8]byte
	_, err := io.ReadFull(reader, encoded[:])
	if errors.Is(err, io.EOF) {
		return provSQLWeightedMember{}, false, nil
	}
	if err != nil {
		return provSQLWeightedMember{}, false, err
	}
	var member provSQLWeightedMember
	copy(member.hash[:], encoded[:sha256.Size])
	member.multiplicity = binary.BigEndian.Uint64(encoded[sha256.Size:])
	if member.multiplicity == 0 {
		return provSQLWeightedMember{}, false, errors.New("ProvSQL weighted witness spill contains zero multiplicity")
	}
	return member, true, nil
}

type provSQLWeightedRun struct {
	file   *os.File
	reader *bufio.Reader
}

func closeProvSQLWeightedRuns(runs []provSQLWeightedRun) {
	for _, run := range runs {
		if run.file != nil {
			_ = run.file.Close()
		}
	}
}

type provSQLWeightedHead struct {
	member provSQLWeightedMember
	run    int
}

type provSQLWeightedHeap []provSQLWeightedHead

func (values provSQLWeightedHeap) Len() int { return len(values) }
func (values provSQLWeightedHeap) Less(i, j int) bool {
	return bytes.Compare(values[i].member.hash[:], values[j].member.hash[:]) < 0
}
func (values provSQLWeightedHeap) Swap(i, j int) { values[i], values[j] = values[j], values[i] }
func (values *provSQLWeightedHeap) Push(value any) {
	*values = append(*values, value.(provSQLWeightedHead))
}
func (values *provSQLWeightedHeap) Pop() any {
	old := *values
	last := old[len(old)-1]
	*values = old[:len(old)-1]
	return last
}

func provSQLFormalLimit(limit int64) bool {
	for _, scale := range provSQLScaleSchedule() {
		if scale.limit == limit {
			return true
		}
	}
	return false
}

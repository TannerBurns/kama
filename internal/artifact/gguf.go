/*
Copyright 2026 Kama Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package artifact

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"path"
	"regexp"
	"slices"
	"strconv"
	"unicode/utf8"
)

const (
	maxGGUFMetadataEntries = 1 << 20
	maxGGUFStringBytes     = 16 << 20
	maxGGUFArrayEntries    = 1 << 24
	maxGGUFTensors         = 1 << 20
	defaultGGUFAlignment   = 32
)

// The enum and block-layout tables below are pinned to llama.cpp/ggml commit
// 571d0d540df04f25298d0e159e520d9fc62ed121 (2026-07-18). Keep the tensor and
// llama file-type enums distinct: their numeric values intentionally differ.

var standardShardPattern = regexp.MustCompile(`^(.*)-(\d{5})-of-(\d{5})\.gguf$`)

type ggufDocument struct {
	Version      uint32
	TensorCount  uint64
	Architecture string
	FileType     *uint32
	ShardNo      *uint32
	ShardCount   *uint32
	Alignment    uint32
}

type ggufTensor struct {
	Name   string
	Offset uint64
	Size   uint64
}

type countingReader struct {
	reader   io.Reader
	count    uint64
	overflow bool
}

func (reader *countingReader) Read(payload []byte) (int, error) {
	read, err := reader.reader.Read(payload)
	if uint64(read) > math.MaxUint64-reader.count {
		reader.overflow = true
	} else {
		reader.count += uint64(read)
	}
	return read, err
}

// InspectGGUF parses the bounded metadata portion of a GGUF v3 file.
func InspectGGUF(reader io.Reader) (GGUFMetadata, error) {
	document, err := parseGGUF(reader)
	if err != nil {
		return GGUFMetadata{}, failure(ReasonInvalidGGUF, "parse GGUF", err)
	}
	if document.Architecture == "" {
		return GGUFMetadata{}, failure(ReasonInvalidGGUF, "parse GGUF", errors.New("general.architecture metadata is missing"))
	}
	if !validGGUFArchitecture(document.Architecture) {
		return GGUFMetadata{}, failure(ReasonInvalidGGUF, "parse GGUF", errors.New("general.architecture is not a bounded canonical identifier"))
	}
	metadata := GGUFMetadata{
		Version:      document.Version,
		Architecture: document.Architecture,
		TensorCount:  document.TensorCount,
	}
	if document.FileType != nil {
		metadata.Quantization = quantizationName(*document.FileType)
	}
	if document.ShardCount != nil {
		metadata.ShardCount = *document.ShardCount
	} else {
		metadata.ShardCount = 1
	}
	return metadata, nil
}

// ValidateGGUFSet securely parses all manifest files and verifies standard
// llama.cpp shard names and split metadata.
//
//nolint:gocyclo // Set validation keeps filename and embedded split invariants together.
func ValidateGGUFSet(root, prefix string, manifest Manifest) (GGUFMetadata, error) {
	if prefix == "." {
		prefix = ""
	}
	if err := ValidateManifest(manifest); err != nil {
		return GGUFMetadata{}, err
	}
	if err := ValidateShardEntrypoint(manifest.Entrypoint); err != nil {
		return GGUFMetadata{}, err
	}
	type inspected struct {
		path     string
		document ggufDocument
	}
	documents := make([]inspected, 0, len(manifest.Files))
	for _, record := range manifest.Files {
		name := record.Path
		if prefix != "" {
			name = path.Join(prefix, name)
		}
		file, err := OpenRegular(root, name)
		if err != nil {
			return GGUFMetadata{}, err
		}
		document, parseErr := parseGGUF(file)
		closeErr := file.Close()
		if parseErr != nil {
			return GGUFMetadata{}, failure(ReasonInvalidGGUF, "parse GGUF "+record.Path, parseErr)
		}
		if closeErr != nil {
			return GGUFMetadata{}, failure(ReasonIOFailure, "close GGUF "+record.Path, closeErr)
		}
		documents = append(documents, inspected{path: record.Path, document: document})
	}

	first := documents[0].document
	if first.Architecture == "" {
		return GGUFMetadata{}, failure(ReasonInvalidGGUF, "validate GGUF metadata", errors.New("general.architecture metadata is missing"))
	}
	if !validGGUFArchitecture(first.Architecture) {
		return GGUFMetadata{}, failure(ReasonInvalidGGUF, "validate GGUF metadata", errors.New("general.architecture is not a bounded canonical identifier"))
	}
	metadata := GGUFMetadata{Version: 3, Architecture: first.Architecture, ShardCount: 1}
	if first.FileType != nil {
		metadata.Quantization = quantizationName(*first.FileType)
	}
	for _, item := range documents {
		if item.document.Architecture != first.Architecture {
			return GGUFMetadata{}, failure(ReasonInvalidGGUF, "validate GGUF metadata", errors.New("shards report different architectures"))
		}
		if !sameUint32(item.document.FileType, first.FileType) {
			return GGUFMetadata{}, failure(ReasonInvalidGGUF, "validate GGUF metadata", errors.New("shards report different file types"))
		}
		if metadata.TensorCount > math.MaxUint64-item.document.TensorCount {
			return GGUFMetadata{}, failure(ReasonInvalidGGUF, "validate GGUF metadata", errors.New("tensor count overflow"))
		}
		metadata.TensorCount += item.document.TensorCount
	}

	parts := standardShardPattern.FindStringSubmatch(path.Base(manifest.Entrypoint))
	if parts == nil {
		if len(documents) != 1 {
			return GGUFMetadata{}, failure(ReasonMissingShard, "validate GGUF shards", errors.New("multiple GGUF files do not use the standard shard naming scheme"))
		}
		if first.ShardCount != nil && *first.ShardCount > 1 {
			return GGUFMetadata{}, failure(ReasonMissingShard, "validate GGUF shards", fmt.Errorf("GGUF metadata declares %d shards but the entrypoint is not standard-sharded", *first.ShardCount))
		}
		return metadata, nil
	}

	count, _ := strconv.Atoi(parts[3])
	if count < 1 || count > MaxSelectedFiles {
		return GGUFMetadata{}, failure(ReasonMissingShard, "validate GGUF shards", fmt.Errorf("declared shard count %d is outside 1..%d", count, MaxSelectedFiles))
	}
	directory := path.Dir(manifest.Entrypoint)
	if directory == "." {
		directory = ""
	}
	expected := make(map[string]int, count)
	for index := 1; index <= count; index++ {
		base := fmt.Sprintf("%s-%05d-of-%05d.gguf", parts[1], index, count)
		name := base
		if directory != "" {
			name = path.Join(directory, base)
		}
		expected[name] = index - 1
	}
	if len(documents) != count {
		return GGUFMetadata{}, failure(ReasonMissingShard, "validate GGUF shards", fmt.Errorf("found %d of %d standard shards", len(documents), count))
	}
	for _, item := range documents {
		index, found := expected[item.path]
		if !found {
			return GGUFMetadata{}, failure(ReasonMissingShard, "validate GGUF shards", fmt.Errorf("unexpected shard %q", item.path))
		}
		if item.document.ShardCount == nil || int(*item.document.ShardCount) != count {
			return GGUFMetadata{}, failure(ReasonMissingShard, "validate GGUF shards", fmt.Errorf("%q has missing or inconsistent split.count", item.path))
		}
		if item.document.ShardNo == nil || int(*item.document.ShardNo) != index {
			return GGUFMetadata{}, failure(ReasonMissingShard, "validate GGUF shards", fmt.Errorf("%q has missing or inconsistent split.no", item.path))
		}
		delete(expected, item.path)
	}
	if len(expected) != 0 {
		return GGUFMetadata{}, failure(ReasonMissingShard, "validate GGUF shards", errors.New("standard shard set is incomplete"))
	}
	metadata.ShardCount = uint32(count)
	return metadata, nil
}

// ExpandStandardShards adds every standard shard implied by entrypoint. The
// caller still verifies that each path exists in its source.
func ExpandStandardShards(entrypoint string, selected []string) ([]string, error) {
	if err := ValidateRelativePath(entrypoint); err != nil {
		return nil, failure(ReasonUnsafePath, "validate shard entrypoint", err)
	}
	if err := ValidateShardEntrypoint(entrypoint); err != nil {
		return nil, err
	}
	if len(selected) == 0 {
		selected = []string{entrypoint}
	}
	parts := standardShardPattern.FindStringSubmatch(path.Base(entrypoint))
	if parts == nil {
		return NormalizeSelectedFiles(entrypoint, selected)
	}
	count, _ := strconv.Atoi(parts[3])
	if count < 1 || count > MaxSelectedFiles {
		return nil, failure(ReasonMissingShard, "expand GGUF shards", fmt.Errorf("declared shard count %d is outside 1..%d", count, MaxSelectedFiles))
	}
	directory := path.Dir(entrypoint)
	if directory == "." {
		directory = ""
	}
	for index := 1; index <= count; index++ {
		name := fmt.Sprintf("%s-%05d-of-%05d.gguf", parts[1], index, count)
		if directory != "" {
			name = path.Join(directory, name)
		}
		selected = append(selected, name)
	}
	return NormalizeSelectedFiles(entrypoint, selected)
}

//nolint:gocyclo // GGUF is an untrusted tagged binary union validated in one bounded parser.
func parseGGUF(reader io.Reader) (ggufDocument, error) {
	counted := &countingReader{reader: reader}
	originalReader := reader
	reader = counted
	var magic [4]byte
	if _, err := io.ReadFull(reader, magic[:]); err != nil {
		return ggufDocument{}, fmt.Errorf("read magic: %w", err)
	}
	if string(magic[:]) != FormatGGUF {
		return ggufDocument{}, fmt.Errorf("invalid GGUF magic %q", magic)
	}
	document := ggufDocument{}
	if err := binary.Read(reader, binary.LittleEndian, &document.Version); err != nil {
		return ggufDocument{}, fmt.Errorf("read version: %w", err)
	}
	if document.Version != 3 {
		return ggufDocument{}, fmt.Errorf("unsupported GGUF version %d", document.Version)
	}
	if err := binary.Read(reader, binary.LittleEndian, &document.TensorCount); err != nil {
		return ggufDocument{}, fmt.Errorf("read tensor count: %w", err)
	}
	if document.TensorCount > maxGGUFTensors {
		return ggufDocument{}, fmt.Errorf("tensor count %d exceeds limit", document.TensorCount)
	}
	document.Alignment = defaultGGUFAlignment
	var metadataCount uint64
	if err := binary.Read(reader, binary.LittleEndian, &metadataCount); err != nil {
		return ggufDocument{}, fmt.Errorf("read metadata count: %w", err)
	}
	if metadataCount > maxGGUFMetadataEntries {
		return ggufDocument{}, fmt.Errorf("metadata count %d exceeds limit", metadataCount)
	}
	for range metadataCount {
		key, err := readGGUFString(reader)
		if err != nil {
			return ggufDocument{}, fmt.Errorf("read metadata key: %w", err)
		}
		var valueType uint32
		if err := binary.Read(reader, binary.LittleEndian, &valueType); err != nil {
			return ggufDocument{}, fmt.Errorf("read metadata type for %q: %w", key, err)
		}
		value, err := readGGUFValue(reader, valueType, 0)
		if err != nil {
			return ggufDocument{}, fmt.Errorf("read metadata value for %q: %w", key, err)
		}
		switch key {
		case "general.architecture":
			if typed, ok := value.(string); ok {
				document.Architecture = typed
			}
		case "general.file_type":
			if typed, ok := numericUint32(value); ok {
				document.FileType = &typed
			}
		case "split.no":
			if typed, ok := numericUint32(value); ok {
				document.ShardNo = &typed
			}
		case "split.count":
			if typed, ok := numericUint32(value); ok {
				document.ShardCount = &typed
			}
		case "general.alignment":
			if typed, ok := numericUint32(value); ok {
				document.Alignment = typed
			}
		}
	}
	if document.Alignment == 0 || document.Alignment%8 != 0 || document.Alignment > 1<<20 {
		return ggufDocument{}, fmt.Errorf("general.alignment %d is invalid", document.Alignment)
	}
	tensors := make([]ggufTensor, 0, document.TensorCount)
	names := make(map[string]struct{}, document.TensorCount)
	for range document.TensorCount {
		name, err := readGGUFString(reader)
		if err != nil {
			return ggufDocument{}, fmt.Errorf("read tensor name: %w", err)
		}
		if name == "" || len(name) > 64 || !utf8.ValidString(name) {
			return ggufDocument{}, errors.New("tensor name must be 1-64 bytes of valid UTF-8")
		}
		if _, exists := names[name]; exists {
			return ggufDocument{}, fmt.Errorf("duplicate tensor name %q", name)
		}
		names[name] = struct{}{}
		var dimensions uint32
		if err := binary.Read(reader, binary.LittleEndian, &dimensions); err != nil {
			return ggufDocument{}, fmt.Errorf("read tensor dimensions for %q: %w", name, err)
		}
		if dimensions == 0 || dimensions > 4 {
			return ggufDocument{}, fmt.Errorf("tensor %q dimension count %d is outside 1..4", name, dimensions)
		}
		shape := make([]uint64, dimensions)
		for index := range shape {
			if err := binary.Read(reader, binary.LittleEndian, &shape[index]); err != nil {
				return ggufDocument{}, fmt.Errorf("read tensor shape for %q: %w", name, err)
			}
			if shape[index] == 0 {
				return ggufDocument{}, fmt.Errorf("tensor %q has a zero dimension", name)
			}
		}
		var tensorType uint32
		var offset uint64
		if err := binary.Read(reader, binary.LittleEndian, &tensorType); err != nil {
			return ggufDocument{}, fmt.Errorf("read tensor type for %q: %w", name, err)
		}
		if err := binary.Read(reader, binary.LittleEndian, &offset); err != nil {
			return ggufDocument{}, fmt.Errorf("read tensor offset for %q: %w", name, err)
		}
		if offset%uint64(document.Alignment) != 0 {
			return ggufDocument{}, fmt.Errorf("tensor %q offset is not %d-byte aligned", name, document.Alignment)
		}
		size, err := ggmlTensorSize(tensorType, shape)
		if err != nil {
			return ggufDocument{}, fmt.Errorf("tensor %q: %w", name, err)
		}
		tensors = append(tensors, ggufTensor{Name: name, Offset: offset, Size: size})
	}
	if len(tensors) > 0 {
		dataStart, ok := alignUint64(counted.count, uint64(document.Alignment))
		if !ok {
			return ggufDocument{}, errors.New("GGUF data offset overflows")
		}
		fileSize, err := ggufReaderSize(originalReader, counted)
		if err != nil {
			return ggufDocument{}, err
		}
		slices.SortFunc(tensors, func(left, right ggufTensor) int {
			if left.Offset < right.Offset {
				return -1
			}
			if left.Offset > right.Offset {
				return 1
			}
			return 0
		})
		previousEnd := uint64(0)
		for _, tensor := range tensors {
			if tensor.Offset < previousEnd {
				return ggufDocument{}, fmt.Errorf("tensor %q overlaps prior tensor data", tensor.Name)
			}
			end, ok := addUint64(tensor.Offset, tensor.Size)
			if !ok {
				return ggufDocument{}, fmt.Errorf("tensor %q data range overflows", tensor.Name)
			}
			absoluteEnd, ok := addUint64(dataStart, end)
			if !ok || absoluteEnd > fileSize {
				return ggufDocument{}, fmt.Errorf("tensor %q data exceeds file bounds", tensor.Name)
			}
			previousEnd = end
		}
	}
	return document, nil
}

func ggmlTensorSize(tensorType uint32, shape []uint64) (uint64, error) {
	type block struct{ elements, bytes uint64 }
	// Values mirror enum ggml_type plus ggml_type_traits at
	// ggmlTableSourceCommit. Removed/repacked-only enum slots are rejected.
	blocks := map[uint32]block{
		0: {1, 4}, 1: {1, 2}, 2: {32, 18}, 3: {32, 20}, 6: {32, 22}, 7: {32, 24},
		8: {32, 34}, 9: {32, 36}, 10: {256, 84}, 11: {256, 110}, 12: {256, 144},
		13: {256, 176}, 14: {256, 210}, 15: {256, 292}, 16: {256, 66}, 17: {256, 74},
		18: {256, 98}, 19: {256, 50}, 20: {32, 18}, 21: {256, 110}, 22: {256, 82},
		23: {256, 136}, 24: {1, 1}, 25: {1, 2}, 26: {1, 4}, 27: {1, 8}, 28: {1, 8},
		29: {256, 56}, 30: {1, 2}, 34: {256, 54}, 35: {256, 66}, 39: {32, 17},
		40: {64, 36}, 41: {128, 18}, 42: {64, 18},
	}
	encoding, found := blocks[tensorType]
	if !found {
		return 0, fmt.Errorf("unsupported GGML tensor type %d", tensorType)
	}
	if shape[0]%encoding.elements != 0 {
		return 0, fmt.Errorf("first dimension %d is not a multiple of type block %d", shape[0], encoding.elements)
	}
	blocksInTensor := shape[0] / encoding.elements
	for _, dimension := range shape[1:] {
		if dimension > math.MaxUint64/blocksInTensor {
			return 0, errors.New("tensor element count overflows")
		}
		blocksInTensor *= dimension
	}
	if blocksInTensor > math.MaxUint64/encoding.bytes {
		return 0, errors.New("tensor byte size overflows")
	}
	return blocksInTensor * encoding.bytes, nil
}

func ggufReaderSize(original io.Reader, counted *countingReader) (uint64, error) {
	if seeker, ok := original.(io.Seeker); ok {
		current, err := seeker.Seek(0, io.SeekCurrent)
		if err != nil {
			return 0, fmt.Errorf("inspect GGUF position: %w", err)
		}
		end, err := seeker.Seek(0, io.SeekEnd)
		if err != nil {
			return 0, fmt.Errorf("inspect GGUF size: %w", err)
		}
		if _, err := seeker.Seek(current, io.SeekStart); err != nil {
			return 0, fmt.Errorf("restore GGUF position: %w", err)
		}
		if end < 0 {
			return 0, errors.New("GGUF size is negative")
		}
		return uint64(end), nil
	}
	_, err := io.Copy(io.Discard, counted)
	if err != nil {
		return 0, fmt.Errorf("scan GGUF tensor data: %w", err)
	}
	if counted.overflow {
		return 0, errors.New("GGUF size overflows")
	}
	return counted.count, nil
}

func alignUint64(value, alignment uint64) (uint64, bool) {
	remainder := value % alignment
	if remainder == 0 {
		return value, true
	}
	return addUint64(value, alignment-remainder)
}

func addUint64(left, right uint64) (uint64, bool) {
	if right > math.MaxUint64-left {
		return 0, false
	}
	return left + right, true
}

func readGGUFString(reader io.Reader) (string, error) {
	var length uint64
	if err := binary.Read(reader, binary.LittleEndian, &length); err != nil {
		return "", err
	}
	if length > maxGGUFStringBytes {
		return "", fmt.Errorf("string length %d exceeds limit", length)
	}
	value := make([]byte, int(length))
	if _, err := io.ReadFull(reader, value); err != nil {
		return "", err
	}
	return string(value), nil
}

//nolint:gocyclo // The switch mirrors the finite GGUF scalar type table.
func readGGUFValue(reader io.Reader, valueType uint32, depth int) (any, error) {
	if depth > 4 {
		return nil, errors.New("GGUF array nesting exceeds limit")
	}
	switch valueType {
	case 0:
		var value uint8
		if err := binary.Read(reader, binary.LittleEndian, &value); err != nil {
			return nil, err
		}
		return value, nil
	case 1:
		var value int8
		if err := binary.Read(reader, binary.LittleEndian, &value); err != nil {
			return nil, err
		}
		return value, nil
	case 2:
		var value uint16
		if err := binary.Read(reader, binary.LittleEndian, &value); err != nil {
			return nil, err
		}
		return value, nil
	case 3:
		var value int16
		if err := binary.Read(reader, binary.LittleEndian, &value); err != nil {
			return nil, err
		}
		return value, nil
	case 4:
		var value uint32
		if err := binary.Read(reader, binary.LittleEndian, &value); err != nil {
			return nil, err
		}
		return value, nil
	case 5:
		var value int32
		if err := binary.Read(reader, binary.LittleEndian, &value); err != nil {
			return nil, err
		}
		return value, nil
	case 6:
		var value float32
		if err := binary.Read(reader, binary.LittleEndian, &value); err != nil {
			return nil, err
		}
		return value, nil
	case 7:
		var value uint8
		if err := binary.Read(reader, binary.LittleEndian, &value); err != nil {
			return nil, err
		}
		if value > 1 {
			return nil, errors.New("invalid boolean value")
		}
		return value == 1, nil
	case 8:
		return readGGUFString(reader)
	case 9:
		var elementType uint32
		var count uint64
		if err := binary.Read(reader, binary.LittleEndian, &elementType); err != nil {
			return nil, err
		}
		if err := binary.Read(reader, binary.LittleEndian, &count); err != nil {
			return nil, err
		}
		if count > maxGGUFArrayEntries {
			return nil, fmt.Errorf("array length %d exceeds limit", count)
		}
		for range count {
			if _, err := readGGUFValue(reader, elementType, depth+1); err != nil {
				return nil, err
			}
		}
		return nil, nil
	case 10:
		var value uint64
		if err := binary.Read(reader, binary.LittleEndian, &value); err != nil {
			return nil, err
		}
		return value, nil
	case 11:
		var value int64
		if err := binary.Read(reader, binary.LittleEndian, &value); err != nil {
			return nil, err
		}
		return value, nil
	case 12:
		var value float64
		if err := binary.Read(reader, binary.LittleEndian, &value); err != nil {
			return nil, err
		}
		return value, nil
	default:
		return nil, fmt.Errorf("unsupported GGUF value type %d", valueType)
	}
}

func numericUint32(value any) (uint32, bool) {
	switch typed := value.(type) {
	case uint8:
		return uint32(typed), true
	case uint16:
		return uint32(typed), true
	case uint32:
		return typed, true
	case uint64:
		return uint32(typed), typed <= math.MaxUint32
	case int8:
		return uint32(typed), typed >= 0
	case int16:
		return uint32(typed), typed >= 0
	case int32:
		return uint32(typed), typed >= 0
	case int64:
		return uint32(typed), typed >= 0 && typed <= math.MaxUint32
	default:
		return 0, false
	}
}

func sameUint32(left, right *uint32) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func quantizationName(fileType uint32) string {
	// general.file_type uses enum llama_ftype, not enum ggml_type. Values
	// mirror include/llama.h at ggmlTableSourceCommit.
	names := map[uint32]string{
		0: "F32", 1: "F16", 2: "Q4_0", 3: "Q4_1", 7: "Q8_0", 8: "Q5_0", 9: "Q5_1",
		10: "Q2_K", 11: "Q3_K_S", 12: "Q3_K_M", 13: "Q3_K_L", 14: "Q4_K_S", 15: "Q4_K_M",
		16: "Q5_K_S", 17: "Q5_K_M", 18: "Q6_K", 19: "IQ2_XXS", 20: "IQ2_XS", 21: "Q2_K_S",
		22: "IQ3_XS", 23: "IQ3_XXS", 24: "IQ1_S", 25: "IQ4_NL", 26: "IQ3_S", 27: "IQ3_M",
		28: "IQ2_S", 29: "IQ2_M", 30: "IQ4_XS", 31: "IQ1_M", 32: "BF16", 36: "TQ1_0",
		37: "TQ2_0", 38: "MXFP4_MOE", 39: "NVFP4", 40: "Q1_0", 41: "Q2_0",
	}
	if name, found := names[fileType]; found {
		return name
	}
	return fmt.Sprintf("FILE_TYPE_%d", fileType)
}

func validGGUFArchitecture(value string) bool {
	if len(value) == 0 || len(value) > 64 || !utf8.ValidString(value) {
		return false
	}
	for index, character := range value {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' ||
			index > 0 && (character == '_' || character == '-' || character == '.') {
			continue
		}
		return false
	}
	return true
}

func shardNames(entrypoint string) ([]string, error) {
	if err := ValidateShardEntrypoint(entrypoint); err != nil {
		return nil, err
	}
	parts := standardShardPattern.FindStringSubmatch(path.Base(entrypoint))
	if parts == nil {
		return []string{entrypoint}, nil
	}
	count, err := strconv.Atoi(parts[3])
	if err != nil || count < 1 || count > MaxSelectedFiles {
		return nil, failure(ReasonMissingShard, "parse shard name", fmt.Errorf("invalid shard count %q", parts[3]))
	}
	dir := path.Dir(entrypoint)
	if dir == "." {
		dir = ""
	}
	names := make([]string, 0, count)
	for index := 1; index <= count; index++ {
		name := fmt.Sprintf("%s-%05d-of-%05d.gguf", parts[1], index, count)
		if dir != "" {
			name = path.Join(dir, name)
		}
		names = append(names, name)
	}
	return names, nil
}

// ValidateShardEntrypoint requires a standard sharded model to identify its
// first part. llama.cpp consumes that first path as the ordered set entrypoint.
func ValidateShardEntrypoint(entrypoint string) error {
	parts := standardShardPattern.FindStringSubmatch(path.Base(entrypoint))
	if parts != nil && parts[2] != "00001" {
		return failure(ReasonInvalidSpec, "validate GGUF entrypoint", errors.New("a standard sharded entrypoint must be shard 00001"))
	}
	return nil
}

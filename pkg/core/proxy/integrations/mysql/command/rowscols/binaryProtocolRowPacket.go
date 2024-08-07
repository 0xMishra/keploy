//go:build linux

// Package rowscols provides encoding and decoding of MySQL row & column packets.
package rowscols

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/utils"
	"go.keploy.io/server/v2/pkg/models/mysql"
	"go.uber.org/zap"
)

//ref: https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_binary_resultset.html#sect_protocol_binary_resultset_row

func DecodeBinaryRow(_ context.Context, _ *zap.Logger, data []byte, columns []*mysql.ColumnDefinition41) (*mysql.BinaryRow, int, error) {

	offset := 0
	row := &mysql.BinaryRow{
		Header: mysql.Header{
			PayloadLength: utils.ReadUint24(data[offset : offset+3]),
			SequenceID:    data[offset+3],
		},
	}
	offset += 4

	if data[offset] != 0x00 {
		return nil, offset, errors.New("malformed binary row packet")
	}
	row.OkAfterRow = true
	offset++

	nullBitmapLen := (len(columns) + 7 + 2) / 8
	nullBitmap := data[offset : offset+nullBitmapLen]
	row.RowNullBuffer = nullBitmap

	offset += nullBitmapLen

	for i, col := range columns {
		if isNull(nullBitmap, i) { // This Null doesn't progress the offset
			row.Values = append(row.Values, mysql.ColumnEntry{
				Type:  mysql.FieldType(col.Type),
				Name:  col.Name,
				Value: nil,
			})
			continue
		}

		value, n, err := readBinaryValue(data[offset:], col)
		if err != nil {
			return nil, offset, err
		}

		row.Values = append(row.Values, mysql.ColumnEntry{
			Type:  mysql.FieldType(col.Type),
			Name:  col.Name,
			Value: value,
		})
		offset += n
	}
	return row, offset, nil
}

func isNull(nullBitmap []byte, index int) bool {
	bytePos := (index + 2) / 8
	bitPos := (index + 2) % 8
	return nullBitmap[bytePos]&(1<<bitPos) != 0
}

func readBinaryValue(data []byte, col *mysql.ColumnDefinition41) (interface{}, int, error) {
	isUnsigned := col.Flags&mysql.UNSIGNED_FLAG != 0

	switch mysql.FieldType(col.Type) {
	case mysql.FieldTypeLong:
		if len(data) < 4 {
			return nil, 0, errors.New("malformed FieldTypeLong value")
		}
		if isUnsigned {
			return uint32(binary.LittleEndian.Uint32(data[:4])), 4, nil
		}
		return int32(binary.LittleEndian.Uint32(data[:4])), 4, nil

	case mysql.FieldTypeString, mysql.FieldTypeVarString, mysql.FieldTypeVarChar, mysql.FieldTypeBLOB, mysql.FieldTypeTinyBLOB, mysql.FieldTypeMediumBLOB, mysql.FieldTypeLongBLOB, mysql.FieldTypeJSON:
		value, _, n, err := utils.ReadLengthEncodedString(data)
		return string(value), n, err

	case mysql.FieldTypeTiny:
		if isUnsigned {
			return uint8(data[0]), 1, nil
		}
		return int8(data[0]), 1, nil

	case mysql.FieldTypeShort, mysql.FieldTypeYear:
		if len(data) < 2 {
			return nil, 0, errors.New("malformed FieldTypeShort value")
		}
		if isUnsigned {
			return uint16(binary.LittleEndian.Uint16(data[:2])), 2, nil
		}
		return int16(binary.LittleEndian.Uint16(data[:2])), 2, nil

	case mysql.FieldTypeLongLong:
		if len(data) < 8 {
			return nil, 0, errors.New("malformed FieldTypeLongLong value")
		}
		if isUnsigned {
			return uint64(binary.LittleEndian.Uint64(data[:8])), 8, nil
		}
		return int64(binary.LittleEndian.Uint64(data[:8])), 8, nil

	case mysql.FieldTypeFloat:
		if len(data) < 4 {
			return nil, 0, errors.New("malformed FieldTypeFloat value")
		}
		return float32(binary.LittleEndian.Uint32(data[:4])), 4, nil

	case mysql.FieldTypeDouble:
		if len(data) < 8 {
			return nil, 0, errors.New("malformed FieldTypeDouble value")
		}
		return float64(binary.LittleEndian.Uint64(data[:8])), 8, nil

	case mysql.FieldTypeDate, mysql.FieldTypeNewDate:
		value, n, err := parseBinaryDate(data)
		return value, n, err

	case mysql.FieldTypeTimestamp, mysql.FieldTypeDateTime:
		value, n, err := parseBinaryDateTime(data)
		return value, n, err

	case mysql.FieldTypeTime:
		value, n, err := parseBinaryTime(data)
		return value, n, err

	default:
		return nil, 0, fmt.Errorf("unsupported column type: %v", col.Type)
	}
}

func parseBinaryDate(b []byte) (interface{}, int, error) {
	if len(b) == 0 {
		return nil, 0, nil
	}
	length := b[0]
	if length == 0 {
		return nil, 1, nil
	}
	year := binary.LittleEndian.Uint16(b[1:3])
	month := b[3]
	day := b[4]
	return fmt.Sprintf("%04d-%02d-%02d", year, month, day), int(length) + 1, nil
}

func parseBinaryDateTime(b []byte) (interface{}, int, error) {
	if len(b) == 0 {
		return nil, 0, nil
	}
	length := b[0]
	if length == 0 {
		return nil, 1, nil
	}
	year := binary.LittleEndian.Uint16(b[1:3])
	month := b[3]
	day := b[4]
	hour := b[5]
	minute := b[6]
	second := b[7]
	if length > 7 {
		microsecond := binary.LittleEndian.Uint32(b[8:12])
		return fmt.Sprintf("%04d-%02d-%02d %02d:%02d:%02d.%06d", year, month, day, hour, minute, second, microsecond), int(length) + 1, nil
	}
	return fmt.Sprintf("%04d-%02d-%02d %02d:%02d:%02d", year, month, day, hour, minute, second), int(length) + 1, nil
}

func parseBinaryTime(b []byte) (interface{}, int, error) {
	if len(b) == 0 {
		return nil, 0, nil
	}
	length := b[0]
	if length == 0 {
		return nil, 1, nil
	}
	isNegative := b[1] == 1
	days := binary.LittleEndian.Uint32(b[2:6])
	hours := b[6]
	minutes := b[7]
	seconds := b[8]
	var microseconds uint32
	if length > 8 {
		microseconds = binary.LittleEndian.Uint32(b[9:13])
	}
	timeString := fmt.Sprintf("%d %02d:%02d:%02d.%06d", days, hours, minutes, seconds, microseconds)
	if isNegative {
		timeString = "-" + timeString
	}
	return timeString, int(length) + 1, nil
}
func EncodeBinaryRow(_ context.Context, _ *zap.Logger, row *mysql.BinaryRow, columns []*mysql.ColumnDefinition41) ([]byte, error) {
	buf := new(bytes.Buffer)

	// Write the packet header
	if err := utils.WriteUint24(buf, row.Header.PayloadLength); err != nil {
		return nil, fmt.Errorf("failed to write PayloadLength: %w", err)
	}
	if err := buf.WriteByte(row.Header.SequenceID); err != nil {
		return nil, fmt.Errorf("failed to write SequenceID: %w", err)
	}

	// Write the row's OK byte
	if err := buf.WriteByte(0x00); err != nil {
		return nil, fmt.Errorf("failed to write OK byte: %w", err)
	}

	// Write the row's NULL bitmap
	if _, err := buf.Write(row.RowNullBuffer); err != nil {
		return nil, fmt.Errorf("failed to write NULL bitmap: %w", err)
	}

	// Write each column's value
	for i, _ := range columns {
		if isNull(row.RowNullBuffer, i) {
			continue
		}

		value := row.Values[i].Value
		switch row.Values[i].Type {
		case mysql.FieldTypeLong:
			var intValue int32
			switch v := value.(type) {
			case int32:
				intValue = v
			case uint32:
				intValue = int32(v)
			default:
				return nil, fmt.Errorf("invalid value type for long field")
			}
			if err := binary.Write(buf, binary.LittleEndian, intValue); err != nil {
				return nil, fmt.Errorf("failed to write int32 value: %w", err)
			}
		case mysql.FieldTypeString, mysql.FieldTypeVarString, mysql.FieldTypeVarChar, mysql.FieldTypeBLOB, mysql.FieldTypeTinyBLOB, mysql.FieldTypeMediumBLOB, mysql.FieldTypeLongBLOB, mysql.FieldTypeJSON:
			strValue, ok := value.(string)
			if !ok {
				return nil, fmt.Errorf("invalid value type for string field")
			}
			if err := utils.WriteLengthEncodedString(buf, strValue); err != nil {
				return nil, fmt.Errorf("failed to write length-encoded string: %w", err)
			}
		case mysql.FieldTypeTiny:
			var intValue int8
			switch v := value.(type) {
			case int8:
				intValue = v
			case uint8:
				intValue = int8(v)
			default:
				return nil, fmt.Errorf("invalid value type for tiny field")
			}
			if err := buf.WriteByte(byte(intValue)); err != nil {
				return nil, fmt.Errorf("failed to write int8 value: %w", err)
			}
		case mysql.FieldTypeShort, mysql.FieldTypeYear:
			var intValue int16
			switch v := value.(type) {
			case int16:
				intValue = v
			case uint16:
				intValue = int16(v)
			default:
				return nil, fmt.Errorf("invalid value type for short field")
			}
			if err := binary.Write(buf, binary.LittleEndian, intValue); err != nil {
				return nil, fmt.Errorf("failed to write int16 value: %w", err)
			}
		case mysql.FieldTypeLongLong:
			var intValue int64
			switch v := value.(type) {
			case int64:
				intValue = v
			case uint64:
				intValue = int64(v)
			default:
				return nil, fmt.Errorf("invalid value type for long long field")
			}
			if err := binary.Write(buf, binary.LittleEndian, intValue); err != nil {
				return nil, fmt.Errorf("failed to write int64 value: %w", err)
			}
		case mysql.FieldTypeFloat:
			floatValue, ok := value.(float32)
			if !ok {
				return nil, fmt.Errorf("invalid value type for float field")
			}
			if err := binary.Write(buf, binary.LittleEndian, floatValue); err != nil {
				return nil, fmt.Errorf("failed to write float32 value: %w", err)
			}
		case mysql.FieldTypeDouble:
			doubleValue, ok := value.(float64)
			if !ok {
				return nil, fmt.Errorf("invalid value type for double field")
			}
			if err := binary.Write(buf, binary.LittleEndian, doubleValue); err != nil {
				return nil, fmt.Errorf("failed to write float64 value: %w", err)
			}
		case mysql.FieldTypeDate, mysql.FieldTypeNewDate, mysql.FieldTypeTimestamp, mysql.FieldTypeDateTime, mysql.FieldTypeTime:
			dateTimeBytes, err := encodeBinaryDateTime(row.Values[i].Type, value)
			if err != nil {
				return nil, fmt.Errorf("failed to encode date/time value: %w", err)
			}
			if _, err := buf.Write(dateTimeBytes); err != nil {
				return nil, fmt.Errorf("failed to write date/time value: %w", err)
			}
		default:
			return nil, fmt.Errorf("unsupported column type: %v", row.Values[i].Type)
		}
	}

	return buf.Bytes(), nil
}

func encodeBinaryDateTime(fieldType mysql.FieldType, value interface{}) ([]byte, error) {
	switch fieldType {
	case mysql.FieldTypeDate, mysql.FieldTypeNewDate:
		// Date format: YYYY-MM-DD
		dateStr, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("invalid value type for date field")
		}
		var year, month, day int
		_, err := fmt.Sscanf(dateStr, "%04d-%02d-%02d", &year, &month, &day)
		if err != nil {
			return nil, fmt.Errorf("failed to parse date string: %w", err)
		}
		buf := new(bytes.Buffer)
		buf.WriteByte(byte(4))
		binary.Write(buf, binary.LittleEndian, uint16(year))
		buf.WriteByte(byte(month))
		buf.WriteByte(byte(day))
		return buf.Bytes(), nil

	case mysql.FieldTypeTimestamp, mysql.FieldTypeDateTime:
		// DateTime format: YYYY-MM-DD HH:MM:SS
		dateTimeStr, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("invalid value type for datetime field")
		}
		var year, month, day, hour, minute, second int
		_, err := fmt.Sscanf(dateTimeStr, "%04d-%02d-%02d %02d:%02d:%02d", &year, &month, &day, &hour, &minute, &second)
		if err != nil {
			return nil, fmt.Errorf("failed to parse datetime string: %w", err)
		}
		buf := new(bytes.Buffer)
		buf.WriteByte(byte(7))
		binary.Write(buf, binary.LittleEndian, uint16(year))
		buf.WriteByte(byte(month))
		buf.WriteByte(byte(day))
		buf.WriteByte(byte(hour))
		buf.WriteByte(byte(minute))
		buf.WriteByte(byte(second))
		return buf.Bytes(), nil

	case mysql.FieldTypeTime:
		// Time format: [-]HH:MM:SS
		timeStr, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("invalid value type for time field")
		}
		var days, hours, minutes, seconds int
		var isNegative bool
		if timeStr[0] == '-' {
			isNegative = true
			timeStr = timeStr[1:]
		}
		_, err := fmt.Sscanf(timeStr, "%d %02d:%02d:%02d", &days, &hours, &minutes, &seconds)
		if err != nil {
			return nil, fmt.Errorf("failed to parse time string: %w", err)
		}
		buf := new(bytes.Buffer)
		buf.WriteByte(byte(8))
		if isNegative {
			buf.WriteByte(1)
		} else {
			buf.WriteByte(0)
		}
		binary.Write(buf, binary.LittleEndian, uint32(days))
		buf.WriteByte(byte(hours))
		buf.WriteByte(byte(minutes))
		buf.WriteByte(byte(seconds))
		return buf.Bytes(), nil

	default:
		return nil, fmt.Errorf("unsupported date/time field type: %v", fieldType)
	}
}

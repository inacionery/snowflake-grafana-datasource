package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
	"github.com/grafana/grafana-plugin-sdk-go/data"
	_ "github.com/snowflakedb/gosnowflake"
	"strconv"
	"strings"
	"time"
)

const rowLimit = 10000

const timeSeriesType = "time series"

func (qc *queryConfigStruct) isTimeSeriesType() bool {
	return qc.QueryType == timeSeriesType
}

type queryConfigStruct struct {
	FinalQuery    string
	QueryType     string
	RawQuery      string
	TimeColumns   []string
	TimeRange     backend.TimeRange
	Interval      time.Duration
	MaxDataPoints int64
	FillMode      string
	FillValue     float64
}

// Constant used to describe the time series fill mode if no value has been seen
const (
	NullFill     = "null"
	PreviousFill = "previous"
	ValueFill    = "value"
)

type queryModel struct {
	QueryText   string   `json:"queryText"`
	QueryType   string   `json:"queryType"`
	TimeColumns []string `json:"timeColumns"`
}

func (qc *queryConfigStruct) fetchData(config *pluginConfig, password string) (result DataQueryResult, err error) {

	connectionString := getConnectionString(config, password)
	db, err := sql.Open("snowflake", connectionString)
	if err != nil {
		log.DefaultLogger.Error("Could not open database", "err", err)
		return result, err
	}
	defer db.Close()

	log.DefaultLogger.Info("Query", "finalQuery", qc.FinalQuery)
	rows, err := db.Query(qc.FinalQuery)
	if err != nil {
		log.DefaultLogger.Error("Could not execute query", "query", qc.FinalQuery, "err", err)
		return result, err
	}
	defer rows.Close()

	columnTypes, err := rows.ColumnTypes()
	if err != nil {
		log.DefaultLogger.Error("Could not get column types", "err", err)
		return result, err
	}
	columnCount := len(columnTypes)

	if columnCount == 0 {
		return result, nil
	}

	table := DataTable{
		Columns: columnTypes,
		Rows:    make([][]interface{}, 0),
	}

	rowCount := 0
	for ; rows.Next(); rowCount++ {
		if rowCount > rowLimit {
			return result, fmt.Errorf("query row limit exceeded, limit %d", rowLimit)
		}
		values, err := qc.transformQueryResult(columnTypes, rows)
		if err != nil {
			return result, err
		}
		table.Rows = append(table.Rows, values)
	}

	err = rows.Err()
	if err != nil {
		log.DefaultLogger.Error("The row scan finished with an error", "err", err)
		return result, err
	}

	result.Tables = append(result.Tables, table)
	return result, nil
}

func (qc *queryConfigStruct) transformQueryResult(columnTypes []*sql.ColumnType, rows *sql.Rows) ([]interface{}, error) {
	values := make([]interface{}, len(columnTypes))
	valuePtrs := make([]interface{}, len(columnTypes))

	for i := 0; i < len(columnTypes); i++ {
		valuePtrs[i] = &values[i]
	}

	if err := rows.Scan(valuePtrs...); err != nil {
		return nil, err
	}

	// convert types from string type to real type
	for i := 0; i < len(columnTypes); i++ {
		log.DefaultLogger.Debug("Type", fmt.Sprintf("%T %v ", values[i], values[i]), columnTypes[i].DatabaseTypeName())

		// Convert time columns when query mode is time series
		if qc.isTimeSeriesType() && equalsIgnoreCase(qc.TimeColumns, columnTypes[i].Name()) {
			if v, err := strconv.ParseFloat(values[i].(string), 64); err == nil {
				values[i] = time.Unix(int64(v), 0)
			} else {
				return nil, fmt.Errorf("Column %s cannot be converted to Time", columnTypes[i].Name())
			}
			continue
		}

		switch columnTypes[i].DatabaseTypeName() {
		case "BOOLEAN":
			if v, err := strconv.ParseBool(values[i].(string)); err == nil {
				values[i] = v
			} else {
				log.DefaultLogger.Info("Rows", "Error converting string to bool", values[i])
			}
		case "TIMESTAMP_LTZ", "TIMESTAMP_NTZ", "TIMESTAMP_TZ", "DATE", "TIME":
			values[i] = values[i].(time.Time)
		case "FIXED":
			precision, _, _ := columnTypes[i].DecimalSize()
			if v, err := strconv.ParseFloat(values[i].(string), 64); err == nil && precision > 1 {
				values[i] = v
			} else if v, err := strconv.ParseInt(values[i].(string), 10, 64); err == nil {
				values[i] = v
			} else {
				log.DefaultLogger.Info("Rows", "Error converting string to int64", values[i])
			}
		case "REAL":
			if v, err := strconv.ParseFloat(values[i].(string), 64); err == nil {
				values[i] = v
			} else {
				log.DefaultLogger.Info("Rows", "Error converting string to float64", values[i])
			}
		case "TEXT":
			if values[i] != nil {
				values[i] = values[i].(string)
			}
		default:
			if values[i] != nil {
				values[i] = values[i].(string)
			}
		}
	}

	return values, nil
}

func (td *SnowflakeDatasource) query(dataQuery backend.DataQuery, config pluginConfig, password string) (response backend.DataResponse) {
	var qm queryModel
	err := json.Unmarshal(dataQuery.JSON, &qm)
	if err != nil {
		log.DefaultLogger.Error("Could not unmarshal query", "err", err)
		response.Error = err
		return response
	}

	if qm.QueryText == "" {
		log.DefaultLogger.Error("SQL query must no be empty")
		response.Error = fmt.Errorf("SQL query must no be empty")
		return response
	}

	queryConfig := queryConfigStruct{
		FinalQuery:    qm.QueryText,
		RawQuery:      qm.QueryText,
		TimeColumns:   qm.TimeColumns,
		QueryType:     dataQuery.QueryType,
		Interval:      dataQuery.Interval,
		TimeRange:     dataQuery.TimeRange,
		MaxDataPoints: dataQuery.MaxDataPoints,
	}

	log.DefaultLogger.Info("Query config", "config", qm)

	// Apply macros
	queryConfig.FinalQuery, err = Interpolate(&queryConfig)
	if err != nil {
		response.Error = err
		return response
	}

	// Add max Datapoint LIMIT option
	if queryConfig.MaxDataPoints > 0 {
		queryConfig.FinalQuery = fmt.Sprintf("%s LIMIT %d", queryConfig.FinalQuery, queryConfig.MaxDataPoints)
	}

	frame := data.NewFrame("response")
	frame.Meta = &data.FrameMeta{ExecutedQueryString: queryConfig.FinalQuery}

	dataResponse, err := queryConfig.fetchData(&config, password)
	if err != nil {
		response.Error = err
		return response
	}
	log.DefaultLogger.Debug("Response", "data", dataResponse)

	for _, table := range dataResponse.Tables {
		timeColumnIndex := -1
		for i, column := range table.Columns {
			// Check time column
			if queryConfig.isTimeSeriesType() && equalsIgnoreCase(queryConfig.TimeColumns, column.Name()) {
				if strings.EqualFold(column.Name(), "Time") {
					timeColumnIndex = i
				}
				frame.Fields = append(frame.Fields, data.NewField(column.Name(), nil, []*time.Time{}))
				continue
			}
			switch column.DatabaseTypeName() {
			case "BOOLEAN":
				frame.Fields = append(frame.Fields, data.NewField(column.Name(), nil, []*bool{}))
			case "TIMESTAMP_LTZ", "TIMESTAMP_NTZ", "TIMESTAMP_TZ", "DATE", "TIME":
				frame.Fields = append(frame.Fields, data.NewField(column.Name(), nil, []*time.Time{}))
			case "FIXED":
				precision, _, _ := column.DecimalSize()
				if precision > 1 {
					frame.Fields = append(frame.Fields, data.NewField(column.Name(), nil, []*float64{}))
				} else {
					frame.Fields = append(frame.Fields, data.NewField(column.Name(), nil, []*int64{}))
				}
			case "REAL":
				frame.Fields = append(frame.Fields, data.NewField(column.Name(), nil, []*float64{}))
			case "TEXT":
				frame.Fields = append(frame.Fields, data.NewField(column.Name(), nil, []*string{}))
			default:
				log.DefaultLogger.Error("Rows", "Unknown database type", column.DatabaseTypeName())
				frame.Fields = append(frame.Fields, data.NewField(column.Name(), nil, []*string{}))
			}
		}

		intervalStart := queryConfig.TimeRange.From.UnixNano() / 1e6
		intervalEnd := queryConfig.TimeRange.To.UnixNano() / 1e6

		count := 0
		// add rows
		for j, row := range table.Rows {
			// Handle fill mode when the time column exist
			if timeColumnIndex != -1 {
				fillTimesSeries(queryConfig, intervalStart, row[Max(int64(timeColumnIndex), 0)].(time.Time).UnixNano()/1e6, timeColumnIndex, frame, len(table.Columns), &count, previousRow(table.Rows, j))
			}
			// without fill mode
			for i, v := range row {
				insertFrameField(frame, v, i)
			}
			count++
		}
		fillTimesSeries(queryConfig, intervalStart, intervalEnd, timeColumnIndex, frame, len(table.Columns), &count, previousRow(table.Rows, len(table.Rows)))
	}

	frame.Meta = &data.FrameMeta{
		Custom: map[string]interface{}{
			"rowCount": len(dataResponse.Tables),
		},
	}

	response.Frames = append(response.Frames, frame)

	return response
}

func fillTimesSeries(queryConfig queryConfigStruct, intervalStart int64, intervalEnd int64, timeColumnIndex int, frame *data.Frame, columnSize int, count *int, previousRow []interface{}) {
	if queryConfig.isTimeSeriesType() && queryConfig.FillMode != "" && timeColumnIndex != -1 {
		for stepTime := intervalStart + queryConfig.Interval.Nanoseconds()/1e6*int64(*count); stepTime < intervalEnd; stepTime = stepTime + (queryConfig.Interval.Nanoseconds() / 1e6) {
			for i := 0; i < columnSize; i++ {
				if i == timeColumnIndex {
					t := time.Unix(stepTime/1e3, 0)
					frame.Fields[i].Append(&t)
					continue
				}
				switch queryConfig.FillMode {
				case ValueFill:
					frame.Fields[i].Append(&queryConfig.FillValue)
				case NullFill:
					frame.Fields[i].Append(nil)
				case PreviousFill:
					if previousRow == nil {
						insertFrameField(frame, nil, i)
					} else {
						insertFrameField(frame, previousRow[i], i)
					}
				default:
				}
			}
			*count++
		}
	}
}

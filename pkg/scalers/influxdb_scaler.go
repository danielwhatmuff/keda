package scalers

import (
	"context"
	"fmt"

	"github.com/InfluxCommunity/influxdb3-go/influxdb3"
	"github.com/go-logr/logr"
	influxdb2 "github.com/influxdata/influxdb-client-go/v2"
	api "github.com/influxdata/influxdb-client-go/v2/api"
	v2 "k8s.io/api/autoscaling/v2"
	"k8s.io/metrics/pkg/apis/external_metrics"
	"strings"

	"github.com/kedacore/keda/v2/pkg/scalers/scalersconfig"
	"github.com/kedacore/keda/v2/pkg/util"
)

type influxDBScaler struct {
	client     influxdb2.Client
	metricType v2.MetricTargetType
	metadata   *influxDBMetadata
	logger     logr.Logger
}

type influxDBScalerV3 struct {
	client     influxdb3.Client
	metricType v2.MetricTargetType
	metadata   *influxDBMetadata
	logger     logr.Logger
}

type influxDBMetadata struct {
	AuthToken                string  `keda:"name=authToken, order=triggerMetadata;resolvedEnv;authParams"`
	OrganizationName         string  `keda:"name=organizationName, order=triggerMetadata;resolvedEnv;authParams"`
	Query                    string  `keda:"name=query, order=triggerMetadata"`
	InfluxVersion            string  `keda:"name=influxVersion, order=triggerMetadata, optional"`
	QueryType                string  `keda:"name=queryType, order=triggerMetadata, optional"`
	Database                 string  `keda:"name=database, order=triggerMetadata;authParams, optional"`
	MetricKey                string  `keda:"name=metricKey, order=triggerMetadata, optional"`
	ServerURL                string  `keda:"name=serverURL, order=triggerMetadata;authParams"`
	UnsafeSsl                bool    `keda:"name=unsafeSsl, order=triggerMetadata, optional"`
	ThresholdValue           float64 `keda:"name=thresholdValue, order=triggerMetadata, optional"`
	ActivationThresholdValue float64 `keda:"name=activationThresholdValue, order=triggerMetadata, optional"`

	triggerIndex int
}

// NewInfluxDBScaler creates a new influx db scaler
func NewInfluxDBScaler(config *scalersconfig.ScalerConfig) (Scaler, error) {
	metricType, err := GetMetricTargetType(config)
	if err != nil {
		return nil, fmt.Errorf("error getting scaler metric type: %w", err)
	}

	logger := InitializeLogger(config, "influxdb_scaler")

	meta, err := parseInfluxDBMetadata(config)
	if err != nil {
		return nil, fmt.Errorf("error parsing influxdb metadata: %w", err)
	}

	if meta.InfluxVersion == "3" {
		logger.V(1).Info("starting up influxdb v3 client")

		clientv3, err := influxdb3.New(influxdb3.ClientConfig{
			Host:     meta.ServerURL,
			Token:    meta.AuthToken,
			Database: meta.Database,
		})

		if err != nil {
			return nil, fmt.Errorf("failed to establish influx v3 client %w", err)
		}

		return &influxDBScalerV3{
			client:     *clientv3,
			metricType: metricType,
			metadata:   meta,
			logger:     logger,
		}, nil
	}

	logger.V(1).Info("starting up influxdb v2 client")

	client := influxdb2.NewClientWithOptions(
		meta.ServerURL,
		meta.AuthToken,
		influxdb2.DefaultOptions().SetTLSConfig(util.CreateTLSClientConfig(meta.UnsafeSsl)))

	return &influxDBScaler{
		client:     client,
		metricType: metricType,
		metadata:   meta,
		logger:     logger,
	}, nil
}

// parseInfluxDBMetadata parses the metadata passed in from the ScaledObject config
func parseInfluxDBMetadata(config *scalersconfig.ScalerConfig) (*influxDBMetadata, error) {
	meta := &influxDBMetadata{}
	meta.triggerIndex = config.TriggerIndex
	if err := config.TypedConfig(meta); err != nil {
		return nil, fmt.Errorf("error parsing influxdb metadata: %w", err)
	}

	if meta.ThresholdValue == 0 && !config.AsMetricSource {
		return nil, fmt.Errorf("no threshold value given")
	}

	return meta, nil
}

// Close closes the connection of the client to the server
func (s *influxDBScaler) Close(context.Context) error {
	s.client.Close()
	return nil
}

// Close closes the connection of the client to the server
func (s *influxDBScalerV3) Close(context.Context) error {
	err := s.client.Close()
	if err != nil {
		return err
	}
	return nil
}

// queryInfluxDB runs the query against the associated influxdb database
// there is an implicit assumption here that the first value returned from the iterator
// will be the value of interest
func queryInfluxDB(ctx context.Context, queryAPI api.QueryAPI, query string) (float64, error) {
	result, err := queryAPI.Query(ctx, query)
	if err != nil {
		return 0, err
	}

	valueExists := result.Next()
	if !valueExists {
		return 0, fmt.Errorf("no results found from query")
	}

	switch valRaw := result.Record().Value().(type) {
	case float64:
		return valRaw, nil
	case int64:
		return float64(valRaw), nil
	default:
		return 0, fmt.Errorf("value of type %T could not be converted into a float", valRaw)
	}
}

// queryOptionsInfluxDBV3 returns influxdb QueryOptions based on the database and queryType (InfluxQL or FlightSQL)
func queryOptionsInfluxDBV3(d string, q string) influxdb3.Option {
	if strings.ToLower(q) == "influxql" {
		return influxdb3.WithQueryType(influxdb3.InfluxQL)
	}
	return influxdb3.WithQueryType(influxdb3.SQL)
}

// queryInfluxDBV3 runs the query against the associated influxdb v3 database
// there is an implicit assumption here that the first value returned from the iterator
// will be the value of interest
func queryInfluxDBV3(ctx context.Context, client influxdb3.Client, metadata influxDBMetadata) (float64, error) {
	queryOption := queryOptionsInfluxDBV3(metadata.Database, metadata.QueryType)

	result, err := client.Query(ctx, metadata.Query, queryOption)

	if err != nil {
		return 0, err
	}

	var parsedVal float64

	for result.Next() {
		value := result.Value()

		switch valRaw := value[metadata.MetricKey].(type) {
		case float64:
			parsedVal = valRaw
		case int64:
			parsedVal = float64(valRaw)
		default:
			return 0, fmt.Errorf("value of type %T could not be converted into a float", valRaw)
		}
	}
	return parsedVal, nil
}

// GetMetricsAndActivity connects to influxdb via the client and returns a value based on the query
func (s *influxDBScaler) GetMetricsAndActivity(ctx context.Context, metricName string) ([]external_metrics.ExternalMetricValue, bool, error) {
	// Grab QueryAPI to make queries to influxdb instance
	queryAPI := s.client.QueryAPI(s.metadata.OrganizationName)

	value, err := queryInfluxDB(ctx, queryAPI, s.metadata.Query)
	if err != nil {
		return []external_metrics.ExternalMetricValue{}, false, err
	}

	metric := GenerateMetricInMili(metricName, value)

	return []external_metrics.ExternalMetricValue{metric}, value > s.metadata.ActivationThresholdValue, nil
}

// GetMetricsAndActivity connects to influxdb via the client and returns a value based on the query
func (s *influxDBScalerV3) GetMetricsAndActivity(ctx context.Context, metricName string) ([]external_metrics.ExternalMetricValue, bool, error) {
	value, err := queryInfluxDBV3(ctx, s.client, *s.metadata)

	if err != nil {
		return []external_metrics.ExternalMetricValue{}, false, err
	}

	metric := GenerateMetricInMili(metricName, value)

	return []external_metrics.ExternalMetricValue{metric}, value > s.metadata.ActivationThresholdValue, nil
}

// GetMetricSpecForScaling returns the metric spec for the Horizontal Pod Autoscaler
func (s *influxDBScaler) GetMetricSpecForScaling(context.Context) []v2.MetricSpec {
	externalMetric := &v2.ExternalMetricSource{
		Metric: v2.MetricIdentifier{
			Name: GenerateMetricNameWithIndex(s.metadata.triggerIndex, util.NormalizeString(fmt.Sprintf("influxdb-%s", s.metadata.OrganizationName))),
		},
		Target: GetMetricTargetMili(s.metricType, s.metadata.ThresholdValue),
	}
	metricSpec := v2.MetricSpec{
		External: externalMetric, Type: externalMetricType,
	}
	return []v2.MetricSpec{metricSpec}
}

// GetMetricSpecForScaling returns the metric spec for the Horizontal Pod Autoscaler
func (s *influxDBScalerV3) GetMetricSpecForScaling(context.Context) []v2.MetricSpec {
	externalMetric := &v2.ExternalMetricSource{
		Metric: v2.MetricIdentifier{
			Name: GenerateMetricNameWithIndex(s.metadata.triggerIndex, util.NormalizeString(fmt.Sprintf("influxdb-%s", s.metadata.Database))),
		},
		Target: GetMetricTargetMili(s.metricType, s.metadata.ThresholdValue),
	}
	metricSpec := v2.MetricSpec{
		External: externalMetric, Type: externalMetricType,
	}
	return []v2.MetricSpec{metricSpec}
}

package metrics

import (
	"reflect"
	"strings"

	"github.com/blang/semver"
	"github.com/newrelic/infra-integrations-sdk/data/metric"
	"github.com/newrelic/infra-integrations-sdk/integration"
	"github.com/newrelic/infra-integrations-sdk/log"
	"github.com/newrelic/nri-postgresql/src/collection"
	"github.com/newrelic/nri-postgresql/src/connection"
)

const (
	versionQuery = `SHOW server_version`
)

// PopulateMetrics collects metrics for each type
func PopulateMetrics(ci connection.Info, databaseList collection.DatabaseList, instance *integration.Entity, i *integration.Integration, collectPgBouncer, collectDbLocks bool) {

	con, err := ci.NewConnection(ci.DatabaseName())
	if err != nil {
		log.Error("Metrics collection failed: error creating connection to SQL Server: %s", err.Error())
		return
	}
	defer con.Close()

	version, err := collectVersion(con)
	if err != nil {
		log.Error("Metrics collection failed: error collecting version number: %s", err.Error())
		return
	}

	PopulateInstanceMetrics(instance, version, con)
	PopulateDatabaseMetrics(databaseList, version, i, con, ci)
	if collectDbLocks {
		PopulateDatabaseLockMetrics(databaseList, version, i, con, ci)
	}
	PopulateTableMetrics(databaseList, i, ci)
	PopulateIndexMetrics(databaseList, i, ci)

	if collectPgBouncer {
		con, err = ci.NewConnection("pgbouncer")
		if err != nil {
			log.Error("Error creating connection to pgbouncer database: %s", err)
		} else {
			defer con.Close()
			PopulatePgBouncerMetrics(i, con, ci)
		}
	}
}

type serverVersionRow struct {
	Version string `db:"server_version"`
}

func collectVersion(connection *connection.PGSQLConnection) (*semver.Version, error) {
	var versionRows []*serverVersionRow
	if err := connection.Query(&versionRows, versionQuery); err != nil {
		return nil, err
	}

	// special cases for ubuntu/debian parsing
	version := versionRows[0].Version
	if strings.Contains(version, "Ubuntu") {
		return parseSpecialVersion(version, strings.Index(version, " (Ubuntu"))
	} else if strings.Contains(version, "Debian") {
		return parseSpecialVersion(version, strings.Index(version, " (Debian"))
	}

	v, err := semver.ParseTolerant(versionRows[0].Version)
	if err != nil {
		return nil, err
	}

	return &v, nil
}

func parseSpecialVersion(version string, specialIndex int) (*semver.Version, error) {
	partialVersion := version[:specialIndex]

	v, err := semver.ParseTolerant(partialVersion)
	if err != nil {
		return nil, err
	}

	return &v, nil
}

// PopulateInstanceMetrics populates the metrics for an instance
func PopulateInstanceMetrics(instanceEntity *integration.Entity, version *semver.Version, connection *connection.PGSQLConnection) {
	metricSet := instanceEntity.NewMetricSet("PostgresqlInstanceSample",
		metric.Attribute{Key: "displayName", Value: instanceEntity.Metadata.Name},
		metric.Attribute{Key: "entityName", Value: instanceEntity.Metadata.Namespace + ":" + instanceEntity.Metadata.Name},
	)

	for _, queryDef := range generateInstanceDefinitions(version) {
		dataModels := queryDef.GetDataModels()
		if err := connection.Query(dataModels, queryDef.GetQuery()); err != nil {
			log.Error("Could not execute instance query: %s", err.Error())
			continue
		}

		vp := reflect.Indirect(reflect.ValueOf(dataModels))

		// Nothing was returned
		if vp.Len() == 0 {
			log.Debug("No data returned from instance query '%s'", queryDef.GetQuery())
			continue
		}

		vpInterface := vp.Index(0).Interface()
		err := metricSet.MarshalMetrics(vpInterface)
		if err != nil {
			log.Error("Could not parse metrics from instance query result: %s", err.Error())
		}
	}
}

// PopulateDatabaseMetrics populates the metrics for a database
func PopulateDatabaseMetrics(databases collection.DatabaseList, version *semver.Version, pgIntegration *integration.Integration, connection *connection.PGSQLConnection, ci connection.Info) {
	databaseDefinitions := generateDatabaseDefinitions(databases, version)
	processDatabaseDefinitions(databaseDefinitions, pgIntegration, connection, ci)
}

// PopulateDatabaseLockMetrics populates the lock metrics for a database
func PopulateDatabaseLockMetrics(databases collection.DatabaseList, version *semver.Version, pgIntegration *integration.Integration, connection *connection.PGSQLConnection, ci connection.Info) {
	if !connection.HaveExtensionInSchema("tablefunc", "public") {
		log.Warn("Crosstab function not available; database lock metric gathering not possible.")
		log.Warn("To enable database lock metrics, enable the 'tablefunc' extension on the public")
		log.Warn("schema of your database. You can do so by:")
		log.Warn("  1. Installing the postgresql contribs package for your OS; and")
		log.Warn("  2. Run the query 'CREATE EXTENSION tablefunc;' against your database's public schema")
		return
	}

	lockDefinitions := generateLockDefinitions(databases, version)

	processDatabaseDefinitions(lockDefinitions, pgIntegration, connection, ci)
}

func processDatabaseDefinitions(definitions []*QueryDefinition, pgIntegration *integration.Integration, connection *connection.PGSQLConnection, ci connection.Info) {
	for _, queryDef := range definitions {
		// collect into model
		dataModels := queryDef.GetDataModels()
		if err := connection.Query(dataModels, queryDef.GetQuery()); err != nil {
			log.Error("Could not execute database query: %s", err.Error())
			continue
		}

		// for each row in the response
		v := reflect.Indirect(reflect.ValueOf(dataModels))
		for i := 0; i < v.Len(); i++ {
			db := v.Index(i).Interface()
			name, err := GetDatabaseName(db)
			if err != nil {
				log.Error("Unable to get database name: %s", err.Error())
			}

			host, port := ci.HostPort()
			hostIDAttribute := integration.NewIDAttribute("host", host)
			portIDAttribute := integration.NewIDAttribute("port", port)
			databaseEntity, err := pgIntegration.Entity(name, "pg-database", hostIDAttribute, portIDAttribute)
			if err != nil {
				log.Error("Failed to get database entity for name %s: %s", name, err.Error())
			}
			metricSet := databaseEntity.NewMetricSet("PostgresqlDatabaseSample",
				metric.Attribute{Key: "displayName", Value: databaseEntity.Metadata.Name},
				metric.Attribute{Key: "entityName", Value: "database:" + databaseEntity.Metadata.Name},
			)

			if err := metricSet.MarshalMetrics(db); err != nil {
				log.Error("Failed to database entity with metrics: %s", err.Error())
			}

		}
	}
}

// PopulateTableMetrics populates the metrics for a table
func PopulateTableMetrics(databases collection.DatabaseList, pgIntegration *integration.Integration, ci connection.Info) {
	for database, schemaList := range databases {
		if len(schemaList) == 0 {
			return
		}

		// Create a new connection to the database
		con, err := ci.NewConnection(database)
		defer con.Close()
		if err != nil {
			log.Error("Failed to connect to database %s: %s", database, err.Error())
			continue
		}

		populateTableMetricsForDatabase(schemaList, con, pgIntegration, ci)
	}
}

func populateTableMetricsForDatabase(schemaList collection.SchemaList, con *connection.PGSQLConnection, pgIntegration *integration.Integration, ci connection.Info) {

	tableDefinitions := generateTableDefinitions(schemaList)

	// collect into model
	for _, definition := range tableDefinitions {

		dataModels := definition.GetDataModels()
		if err := con.Query(dataModels, definition.GetQuery()); err != nil {
			log.Error("Could not execute table query: %s", err.Error())
			return
		}

		// for each row in the response
		v := reflect.Indirect(reflect.ValueOf(dataModels))
		for i := 0; i < v.Len(); i++ {
			row := v.Index(i).Interface()
			dbName, err := GetDatabaseName(row)
			if err != nil {
				log.Error("Unable to get database name: %s", err.Error())
			}
			schemaName, err := GetSchemaName(row)
			if err != nil {
				log.Error("Unable to get schema name: %s", err.Error())
			}
			tableName, err := GetTableName(row)
			if err != nil {
				log.Error("Unable to get table name: %s", err.Error())
			}

			host, port := ci.HostPort()
			hostIDAttribute := integration.NewIDAttribute("host", host)
			portIDAttribute := integration.NewIDAttribute("port", port)
			databaseIDAttribute := integration.NewIDAttribute("pg-database", dbName)
			schemaIDAttribute := integration.NewIDAttribute("pg-schema", schemaName)
			tableEntity, err := pgIntegration.Entity(tableName, "pg-table", hostIDAttribute, portIDAttribute, databaseIDAttribute, schemaIDAttribute)
			if err != nil {
				log.Error("Failed to get table entity for table %s: %s", tableName, err.Error())
			}
			metricSet := tableEntity.NewMetricSet("PostgresqlTableSample",
				metric.Attribute{Key: "displayName", Value: tableEntity.Metadata.Name},
				metric.Attribute{Key: "entityName", Value: "table:" + tableEntity.Metadata.Name},
				metric.Attribute{Key: "database", Value: dbName},
				metric.Attribute{Key: "schema", Value: schemaName},
			)

			if err := metricSet.MarshalMetrics(row); err != nil {
				log.Error("Failed to populate table entity with metrics: %s", err.Error())
			}

		}
	}
}

// PopulateIndexMetrics populates the metrics for an index
func PopulateIndexMetrics(databases collection.DatabaseList, pgIntegration *integration.Integration, ci connection.Info) {
	for database, schemaList := range databases {
		con, err := ci.NewConnection(database)
		if err != nil {
			log.Error("Failed to create new connection to database %s: %s", database, err.Error())
			continue
		}
		defer con.Close()
		populateIndexMetricsForDatabase(schemaList, con, pgIntegration, ci)
	}
}

func populateIndexMetricsForDatabase(schemaList collection.SchemaList, con *connection.PGSQLConnection, pgIntegration *integration.Integration, ci connection.Info) {
	indexDefinitions := generateIndexDefinitions(schemaList)

	for _, definition := range indexDefinitions {

		// collect into model
		dataModels := definition.GetDataModels()
		if err := con.Query(dataModels, definition.GetQuery()); err != nil {
			log.Error("Could not execute index query: %s", err.Error())
			return
		}

		// for each row in the response
		v := reflect.Indirect(reflect.ValueOf(dataModels))
		for i := 0; i < v.Len(); i++ {
			row := v.Index(i).Interface()
			dbName, err := GetDatabaseName(row)
			if err != nil {
				log.Error("Unable to get database name: %s", err.Error())
			}
			schemaName, err := GetSchemaName(row)
			if err != nil {
				log.Error("Unable to get schema name: %s", err.Error())
			}
			tableName, err := GetTableName(row)
			if err != nil {
				log.Error("Unable to get table name: %s", err.Error())
			}
			indexName, err := GetIndexName(row)
			if err != nil {
				log.Error("Unable to get index name: %s", err.Error())
			}

			host, port := ci.HostPort()
			hostIDAttribute := integration.NewIDAttribute("host", host)
			portIDAttribute := integration.NewIDAttribute("port", port)
			databaseIDAttribute := integration.NewIDAttribute("pg-database", dbName)
			schemaIDAttribute := integration.NewIDAttribute("pg-schema", schemaName)
			tableIDAttribute := integration.NewIDAttribute("pg-table", tableName)
			indexEntity, err := pgIntegration.Entity(indexName, "pg-index", hostIDAttribute, portIDAttribute, databaseIDAttribute, schemaIDAttribute, tableIDAttribute)
			if err != nil {
				log.Error("Failed to get table entity for index %s: %s", indexName, err.Error())
			}
			metricSet := indexEntity.NewMetricSet("PostgresqlIndexSample",
				metric.Attribute{Key: "displayName", Value: indexEntity.Metadata.Name},
				metric.Attribute{Key: "entityName", Value: "index:" + indexEntity.Metadata.Name},
				metric.Attribute{Key: "database", Value: dbName},
				metric.Attribute{Key: "schema", Value: schemaName},
				metric.Attribute{Key: "table", Value: tableName},
			)

			if err := metricSet.MarshalMetrics(row); err != nil {
				log.Error("Failed to populate index entity with metrics: %s", err.Error())
			}

		}

	}
}

// PopulatePgBouncerMetrics populates pgbouncer metrics
func PopulatePgBouncerMetrics(pgIntegration *integration.Integration, con *connection.PGSQLConnection, ci connection.Info) {
	pgbouncerDefs := generatePgBouncerDefinitions()

	for _, definition := range pgbouncerDefs {
		dataModels := definition.GetDataModels()
		if err := con.Query(dataModels, definition.GetQuery()); err != nil {
			log.Error("Could not execute index query: %s", err.Error())
			return
		}

		// for each row in the response
		v := reflect.Indirect(reflect.ValueOf(dataModels))
		for i := 0; i < v.Len(); i++ {
			db := v.Index(i).Interface()
			name, err := GetDatabaseName(db)
			if err != nil {
				log.Error("Unable to get database name: %s", err.Error())
				continue
			}

			host, port := ci.HostPort()
			hostIDAttribute := integration.NewIDAttribute("host", host)
			portIDAttribute := integration.NewIDAttribute("port", port)
			pgEntity, err := pgIntegration.Entity(name, "pgbouncer", hostIDAttribute, portIDAttribute)
			if err != nil {
				log.Error("Failed to get database entity for name %s: %s", name, err.Error())
			}
			metricSet := pgEntity.NewMetricSet("PgBouncerSample",
				metric.Attribute{Key: "displayName", Value: name},
				metric.Attribute{Key: "entityName", Value: "pgbouncer:" + name},
				metric.Attribute{Key: "host", Value: host},
			)

			if err := metricSet.MarshalMetrics(db); err != nil {
				log.Error("Failed to populate pgbouncer entity with metrics: %s", err.Error())
			}

		}
	}
}

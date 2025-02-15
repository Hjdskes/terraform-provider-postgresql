package postgresql

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/hashicorp/errwrap"
	"github.com/hashicorp/terraform/helper/resource"
)

const (
	dbNamePrefix     = "tf_tests_db"
	roleNamePrefix   = "tf_tests_role"
	testRolePassword = "testpwd"
)

// Can be used in a PreCheck function to disable test based on feature.
func testCheckCompatibleVersion(t *testing.T, feature featureName) {
	client := testAccProvider.Meta().(*Client)
	if !client.featureSupported(feature) {
		t.Skip(fmt.Sprintf("Skip extension tests for Postgres %s", client.version))
	}
}

func getTestConfig(t *testing.T) Config {
	getEnv := func(key, fallback string) string {
		value := os.Getenv(key)
		if len(value) == 0 {
			return fallback
		}
		return value
	}

	dbPort, err := strconv.Atoi(getEnv("PGPORT", "5432"))
	if err != nil {
		t.Fatalf("could not cast PGPORT value as integer: %v", err)
	}

	return Config{
		Host:     getEnv("PGHOST", "localhost"),
		Port:     dbPort,
		Username: getEnv("PGUSER", ""),
		Password: getEnv("PGPASSWORD", ""),
		SSLMode:  getEnv("PGSSLMODE", ""),
	}
}

func skipIfNotAcc(t *testing.T) {
	if os.Getenv(resource.TestEnvVar) == "" {
		t.Skip(fmt.Sprintf(
			"Acceptance tests skipped unless env '%s' set",
			resource.TestEnvVar))
	}
}

// dbExecute is a test helper to create a pool, execute one query then close the pool
func dbExecute(t *testing.T, dsn, query string, args ...interface{}) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("could to create connection pool: %v", err)
	}
	defer db.Close()

	// Create the test DB
	if _, err = db.Exec(query, args...); err != nil {
		t.Fatalf("could not execute query %s: %v", query, err)
	}
}

func getTestDBNames(dbSuffix string) (dbName string, roleName string) {
	dbName = fmt.Sprintf("%s_%s", dbNamePrefix, dbSuffix)
	roleName = fmt.Sprintf("%s_%s", roleNamePrefix, dbSuffix)

	return
}

// setupTestDatabase creates all needed resources before executing a terraform test
// and provides the teardown function to delete all these resources.
func setupTestDatabase(t *testing.T, createDB, createRole bool) (string, func()) {
	config := getTestConfig(t)

	suffix := strconv.Itoa(int(time.Now().UnixNano()))

	dbName, roleName := getTestDBNames(suffix)

	if createRole {
		dbExecute(t, config.connStr("postgres"), fmt.Sprintf(
			"CREATE ROLE %s LOGIN ENCRYPTED PASSWORD '%s'",
			roleName, testRolePassword,
		))
	}

	if createDB {
		dbExecute(t, config.connStr("postgres"), fmt.Sprintf("CREATE DATABASE %s", dbName))
		// Create a test schema in this new database and grant usage to rolName
		dbExecute(t, config.connStr(dbName), "CREATE SCHEMA test_schema")
		dbExecute(t, config.connStr(dbName), fmt.Sprintf("GRANT usage ON SCHEMA test_schema to %s", roleName))
	}

	return suffix, func() {
		dbExecute(t, config.connStr("postgres"), fmt.Sprintf("DROP DATABASE IF EXISTS %s", dbName))
		dbExecute(t, config.connStr("postgres"), fmt.Sprintf("DROP ROLE IF EXISTS %s", roleName))
	}
}

func createTestTables(t *testing.T, dbSuffix string, tables []string, owner string) func() {
	config := getTestConfig(t)
	dbName, _ := getTestDBNames(dbSuffix)

	db, err := sql.Open("postgres", config.connStr(dbName))
	if err != nil {
		t.Fatalf("could not open connection pool for db %s: %v", dbName, err)
	}
	defer db.Close()

	if owner != "" {
		if _, err := db.Exec(fmt.Sprintf("SET ROLE %s", owner)); err != nil {
			t.Fatalf("could not set role to %s: %v", owner, err)
		}
	}

	for _, table := range tables {
		if _, err := db.Exec(fmt.Sprintf("CREATE TABLE %s (val text)", table)); err != nil {
			t.Fatalf("could not create test table in db %s: %v", dbName, err)
		}
		if owner != "" {
			if _, err := db.Exec(fmt.Sprintf("ALTER TABLE %s OWNER TO %s", table, owner)); err != nil {
				t.Fatalf("could not set test_table owner to %s: %v", owner, err)
			}
		}
	}
	// In this case we need to drop table after each test.
	return func() {
		db, err := sql.Open("postgres", config.connStr(dbName))
		defer db.Close()

		if err != nil {
			t.Fatalf("could not open connection pool for db %s: %v", dbName, err)
		}

		for _, table := range tables {
			if _, err := db.Exec(fmt.Sprintf("DROP TABLE %s", table)); err != nil {
				log.Fatalf("could not drop table %s: %v", table, err)
			}
		}
	}
}

func testCheckTablesPrivileges(t *testing.T, dbSuffix string, tables []string, allowedPrivileges []string) error {
	config := getTestConfig(t)
	dbName, roleName := getTestDBNames(dbSuffix)

	// Connect as the test role
	config.Username = roleName
	config.Password = testRolePassword

	db, err := sql.Open("postgres", config.connStr(dbName))
	if err != nil {
		t.Fatalf("could not open connection pool for db %s: %v", dbName, err)
	}
	defer db.Close()

	for _, table := range tables {
		queries := map[string]string{
			"SELECT": fmt.Sprintf("SELECT count(*) FROM %s", table),
			"INSERT": fmt.Sprintf("INSERT INTO %s VALUES ('test')", table),
			"UPDATE": fmt.Sprintf("UPDATE %s SET val = 'test'", table),
			"DELETE": fmt.Sprintf("DELETE FROM %s", table),
		}

		for queryType, query := range queries {
			_, err := db.Exec(query)

			if err != nil && sliceContainsStr(allowedPrivileges, queryType) {
				return errwrap.Wrapf(
					fmt.Sprintf("could not %s on test table %s: {{err}}", queryType, table),
					err,
				)

			} else if err == nil && !sliceContainsStr(allowedPrivileges, queryType) {
				return errwrap.Wrapf(
					fmt.Sprintf("%s did not failed as expected for table %s: {{err}}", queryType, table),
					err,
				)
			}
		}
	}
	return nil
}

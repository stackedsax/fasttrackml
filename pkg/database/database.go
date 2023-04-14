package database

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	glog "log"
	"net/url"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"gorm.io/plugin/dbresolver"
)

type DbInstance struct {
	*gorm.DB
	dsn     string
	closers []io.Closer
}

func (db *DbInstance) Close() error {
	for _, c := range db.closers {
		err := c.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func (db *DbInstance) DSN() string {
	return db.dsn
}

var DB *DbInstance = &DbInstance{}

func ConnectDB(dsn string, slowThreshold time.Duration, poolMax int, reset bool, migrate bool, artifactRoot string) error {
	DB.dsn = dsn
	var sourceConn gorm.Dialector
	var replicaConn gorm.Dialector
	u, err := url.Parse(dsn)
	if err != nil {
		return fmt.Errorf("invalid database URL: %w", err)
	}
	switch u.Scheme {
	case "postgres", "postgresql":
		sourceConn = postgres.Open(u.String())
	case "sqlite":
		q := u.Query()
		q.Set("_case_sensitive_like", "true")
		q.Set("_mutex", "no")
		if q.Get("mode") != "memory" && !(q.Has("_journal") || q.Has("_journal_mode")) {
			q.Set("_journal", "WAL")
		}
		u.RawQuery = q.Encode()

		s, err := sql.Open(sqlite.DriverName, strings.Replace(u.String(), "sqlite://", "file:", 1))
		if err != nil {
			return fmt.Errorf("failed to connect to database: %w", err)
		}
		DB.closers = append(DB.closers, s)
		s.SetMaxIdleConns(1)
		s.SetMaxOpenConns(4)
		s.SetConnMaxIdleTime(0)
		s.SetConnMaxLifetime(0)
		sourceConn = sqlite.Dialector{
			Conn: s,
		}

		q.Set("_query_only", "true")
		u.RawQuery = q.Encode()
		r, err := sql.Open(sqlite.DriverName, strings.Replace(u.String(), "sqlite://", "file:", 1))
		if err != nil {
			return fmt.Errorf("failed to connect to database: %w", err)
		}
		DB.closers = append(DB.closers, r)
		replicaConn = sqlite.Dialector{
			Conn: r,
		}
	default:
		return fmt.Errorf("unsupported database scheme %s", u.Scheme)
	}

	log.Infof("Using database %s", dsn)

	dbLogLevel := logger.Warn
	if log.GetLevel() == log.DebugLevel {
		dbLogLevel = logger.Info
	}
	DB.DB, err = gorm.Open(sourceConn, &gorm.Config{
		Logger: logger.New(
			glog.New(
				log.StandardLogger().WriterLevel(log.WarnLevel),
				"",
				0,
			),
			logger.Config{
				SlowThreshold:             slowThreshold,
				LogLevel:                  dbLogLevel,
				IgnoreRecordNotFoundError: true,
			},
		),
	})
	if err != nil {
		return fmt.Errorf("failed to connect to database: %w", err)
	}

	if replicaConn != nil {
		DB.Use(
			dbresolver.Register(dbresolver.Config{
				Replicas: []gorm.Dialector{
					replicaConn,
				},
			}),
		)
	}

	if u.Scheme != "sqlite" {
		sqlDB, _ := DB.DB.DB()
		sqlDB.SetConnMaxIdleTime(time.Minute)
		sqlDB.SetMaxIdleConns(poolMax)
		sqlDB.SetMaxOpenConns(poolMax)
	}

	if reset {
		switch u.Scheme {
		case "postgres", "postgresql":
			log.Info("Resetting database")
			DB.Exec("drop schema public cascade")
			DB.Exec("create schema public")
		default:
			return fmt.Errorf("unable to reset database with scheme \"%s\"", u.Scheme)
		}
	}

	var alembicVersion AlembicVersion
	var schemaVersion SchemaVersion
	{
		tx := DB.Session(&gorm.Session{
			Logger: logger.Discard,
		})
		tx.First(&alembicVersion)
		tx.First(&schemaVersion)
	}

	if alembicVersion.Version != "97727af70f4d" || schemaVersion.Version != "8073e7e037e5" {
		if !migrate && alembicVersion.Version != "" {
			return fmt.Errorf("unsupported database schema versions alembic %s, fasttrack %s", alembicVersion.Version, schemaVersion.Version)
		}

		switch alembicVersion.Version {
		case "c48cb773bb87":
			log.Info("Migrating database to alembic schema bd07f7e963c5")
			if err := DB.Transaction(func(tx *gorm.DB) error {
				for _, table := range []any{
					&Param{},
					&Metric{},
					&LatestMetric{},
					&Tag{},
				} {
					if err := tx.Migrator().CreateIndex(table, "RunID"); err != nil {
						return err
					}
				}
				return tx.Model(&AlembicVersion{}).
					Where("1 = 1").
					Update("Version", "bd07f7e963c5").
					Error
			}); err != nil {
				return fmt.Errorf("error migrating database to alembic schema bd07f7e963c5: %w", err)
			}
			fallthrough

		case "bd07f7e963c5":
			log.Info("Migrating database to alembic schema 0c779009ac13")
			if err := DB.Transaction(func(tx *gorm.DB) error {
				if err := tx.Migrator().AddColumn(&Run{}, "DeletedTime"); err != nil {
					return err
				}
				return tx.Model(&AlembicVersion{}).
					Where("1 = 1").
					Update("Version", "0c779009ac13").
					Error
			}); err != nil {
				return fmt.Errorf("error migrating database to alembic schema 0c779009ac13: %w", err)
			}
			fallthrough

		case "0c779009ac13":
			log.Info("Migrating database to alembic schema cc1f77228345")
			if err := DB.Transaction(func(tx *gorm.DB) error {
				if err := tx.Migrator().AlterColumn(&Param{}, "value"); err != nil {
					return err
				}
				return tx.Model(&AlembicVersion{}).
					Where("1 = 1").
					Update("Version", "cc1f77228345").
					Error
			}); err != nil {
				return fmt.Errorf("error migrating database to alembic schema cc1f77228345: %w", err)
			}
			fallthrough

		case "cc1f77228345":
			log.Info("Migrating database to alembic schema 97727af70f4d")
			if err := DB.Transaction(func(tx *gorm.DB) error {
				for _, column := range []string{
					"CreationTime",
					"LastUpdateTime",
				} {
					if err := tx.Migrator().AddColumn(&Experiment{}, column); err != nil {
						return err
					}
				}
				return tx.Model(&AlembicVersion{}).
					Where("1 = 1").
					Update("Version", "97727af70f4d").
					Error
			}); err != nil {
				return fmt.Errorf("error migrating database to alembic schema 97727af70f4d: %w", err)
			}
			fallthrough

		case "97727af70f4d":
			switch schemaVersion.Version {
			case "":
				log.Info("Migrating database to fasttrack schema ac0b8b7c0014")
				if err := DB.Transaction(func(tx *gorm.DB) error {
					for _, column := range []struct {
						dst   any
						field string
					}{
						{&Run{}, "RowNum"},
						{&Metric{}, "Iter"},
						{&LatestMetric{}, "LastIter"},
					} {
						if err := tx.Migrator().AddColumn(column.dst, column.field); err != nil {
							return err
						}
					}
					if err := tx.Exec(
						"UPDATE runs" +
							"  SET row_num = rows.row_num" +
							"  FROM (" +
							"    SELECT run_uuid, ROW_NUMBER() OVER (ORDER BY start_time, run_uuid DESC) - 1 AS row_num" +
							"    FROM runs" +
							"  ) AS rows" +
							"  WHERE runs.run_uuid = rows.run_uuid").
						Error; err != nil {
						return err
					}
					if err := tx.Exec(
						"UPDATE metrics" +
							"  SET iter = iters.iter" +
							"  FROM (" +
							"    SELECT ROW_NUMBER() OVER (PARTITION BY run_uuid, key ORDER BY timestamp, step, value) - 1 AS iter," +
							"      run_uuid, key, timestamp, step, value" +
							"    FROM metrics" +
							"  ) AS iters" +
							"  WHERE" +
							"    (metrics.run_uuid, metrics.key, metrics.timestamp, metrics.step, metrics.value) =" +
							"    (iters.run_uuid, iters.key, iters.timestamp, iters.step, iters.value)").
						Error; err != nil {
						return err
					}
					if err := tx.Exec(
						"UPDATE latest_metrics" +
							"  SET last_iter = metrics.last_iter" +
							"  FROM (" +
							"    SELECT run_uuid, key, MAX(iter) AS last_iter" +
							"    FROM metrics" +
							"    GROUP BY run_uuid, key" +
							"  ) AS metrics" +
							"  WHERE" +
							"    (latest_metrics.run_uuid, latest_metrics.key) =" +
							"    (metrics.run_uuid, metrics.key)").
						Error; err != nil {
						return err
					}
					if err := tx.AutoMigrate(&SchemaVersion{}); err != nil {
						return err
					}
					return tx.Create(&SchemaVersion{
						Version: "ac0b8b7c0014",
					}).Error
				}); err != nil {
					return fmt.Errorf("error migrating database to fasttrack schema ac0b8b7c0014: %w", err)
				}
				fallthrough

			case "ac0b8b7c0014":
				log.Info("Migrating database to fasttrack schema 8073e7e037e5")
				if err := DB.Transaction(func(tx *gorm.DB) error {
					if err := tx.AutoMigrate(
						&Dashboard{},
						&App{},
					); err != nil {
						return err
					}
					return tx.Model(&SchemaVersion{}).
						Where("1 = 1").
						Update("Version", "8073e7e037e5").
						Error
				}); err != nil {
					return fmt.Errorf("error migrating database to fasttrack schema 8073e7e037e5: %w", err)
				}

			default:
				return fmt.Errorf("unsupported database fasttrack schema version %s", schemaVersion.Version)
			}

			log.Info("Database migration done")

		case "":
			log.Info("Initializing database")
			tx := DB.Begin()
			if err := tx.AutoMigrate(
				&Experiment{},
				&ExperimentTag{},
				&Run{},
				&Param{},
				&Tag{},
				&Metric{},
				&LatestMetric{},
				&AlembicVersion{},
				&Dashboard{},
				&App{},
				&SchemaVersion{},
			); err != nil {
				return fmt.Errorf("error initializing database: %w", err)
			}
			tx.Create(&AlembicVersion{
				Version: "97727af70f4d",
			})
			tx.Create(&SchemaVersion{
				Version: "8073e7e037e5",
			})
			tx.Commit()
			if tx.Error != nil {
				return fmt.Errorf("error initializing database: %w", tx.Error)
			}

		default:
			return fmt.Errorf("unsupported database alembic schema version %s", alembicVersion.Version)
		}
	}

	if tx := DB.First(&Experiment{}, 0); tx.Error != nil {
		if errors.Is(tx.Error, gorm.ErrRecordNotFound) {
			log.Info("Creating default experiment")
			var id int32 = 0
			ts := time.Now().UTC().UnixMilli()
			exp := Experiment{
				ID:             &id,
				Name:           "Default",
				LifecycleStage: LifecycleStageActive,
				CreationTime: sql.NullInt64{
					Int64: ts,
					Valid: true,
				},
				LastUpdateTime: sql.NullInt64{
					Int64: ts,
					Valid: true,
				},
			}
			if tx := DB.Create(&exp); tx.Error != nil {
				return fmt.Errorf("error creating default experiment: %s", tx.Error)
			}

			exp.ArtifactLocation = fmt.Sprintf("%s/%d", strings.TrimRight(artifactRoot, "/"), *exp.ID)
			if tx := DB.Model(&exp).Update("ArtifactLocation", exp.ArtifactLocation); tx.Error != nil {
				return fmt.Errorf("error updating artifact_location for experiment '%s': %s", exp.Name, tx.Error)
			}
		} else {
			return fmt.Errorf("unable to find default experiment: %s", tx.Error)
		}
	}

	return nil
}
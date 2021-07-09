package db

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/cheggaaa/pb/v3"
	"github.com/kotakanbe/go-cpe-dictionary/config"
	"github.com/kotakanbe/go-cpe-dictionary/models"
	sqlite3 "github.com/mattn/go-sqlite3"
	"golang.org/x/xerrors"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Supported DB dialects.
const (
	dialectSqlite3    = "sqlite3"
	dialectMysql      = "mysql"
	dialectPostgreSQL = "postgres"
)

// RDBDriver is Driver for RDB
type RDBDriver struct {
	name string
	conn *gorm.DB
}

// Name return db name
func (r *RDBDriver) Name() string {
	return r.name
}

// OpenDB opens Database
func (r *RDBDriver) OpenDB(dbType, dbPath string, debugSQL bool) (locked bool, err error) {
	gormConfig := gorm.Config{
		DisableForeignKeyConstraintWhenMigrating: true,
		Logger:                                   logger.Default.LogMode(logger.Silent),
	}

	if debugSQL {
		gormConfig.Logger = logger.New(
			log.New(os.Stdout, "\r\n", log.LstdFlags),
			logger.Config{
				SlowThreshold: time.Second,
				LogLevel:      logger.Info,
				Colorful:      true,
			},
		)
	}

	switch r.name {
	case dialectSqlite3:
		r.conn, err = gorm.Open(sqlite.Open(dbPath), &gormConfig)
	case dialectMysql:
		r.conn, err = gorm.Open(mysql.Open(dbPath), &gormConfig)
	case dialectPostgreSQL:
		r.conn, err = gorm.Open(postgres.Open(dbPath), &gormConfig)
	default:
		err = xerrors.Errorf("Not Supported DB dialects. r.name: %s", r.name)
	}

	if err != nil {
		msg := fmt.Sprintf("Failed to open DB. dbtype: %s, dbpath: %s, err: %s", dbType, dbPath, err)
		if r.name == dialectSqlite3 {
			switch err.(sqlite3.Error).Code {
			case sqlite3.ErrLocked, sqlite3.ErrBusy:
				return true, fmt.Errorf(msg)
			}
		}
		return false, fmt.Errorf(msg)
	}

	if r.name == dialectSqlite3 {
		r.conn.Exec("PRAGMA foreign_keys = ON")
	}
	return false, nil
}

// CloseDB close Database
func (r *RDBDriver) CloseDB() (err error) {
	if r.conn == nil {
		return
	}

	var sqlDB *sql.DB
	if sqlDB, err = r.conn.DB(); err != nil {
		return xerrors.Errorf("Failed to get DB Object. err : %w", err)
	}
	if err = sqlDB.Close(); err != nil {
		return xerrors.Errorf("Failed to close DB. Type: %s. err: %w", r.name, err)
	}
	return
}

// MigrateDB migrates Database
func (r *RDBDriver) MigrateDB() error {
	if err := r.conn.AutoMigrate(
		&models.FetchMeta{},
		&models.CategorizedCpe{},
	); err != nil {
		return fmt.Errorf("Failed to migrate. err: %s", err)
	}
	return nil
}

// IsGoCPEDictModelV1 determines if the DB was created at the time of go-cpe-dictionary Model v1
func (r *RDBDriver) IsGoCPEDictModelV1() (bool, error) {
	if r.conn.Migrator().HasTable(&models.FetchMeta{}) {
		return false, nil
	}

	var (
		count int64
		err   error
	)
	switch r.name {
	case dialectSqlite3:
		err = r.conn.Table("sqlite_master").Where("type = ?", "table").Count(&count).Error
	case dialectMysql:
		err = r.conn.Table("information_schema.tables").Where("table_schema = ?", r.conn.Migrator().CurrentDatabase()).Count(&count).Error
	case dialectPostgreSQL:
		err = r.conn.Table("pg_tables").Where("schemaname = ?", "public").Count(&count).Error
	}

	if count > 0 {
		return true, nil
	}
	return false, err
}

// GetFetchMeta get FetchMeta from Database
func (r *RDBDriver) GetFetchMeta() (fetchMeta *models.FetchMeta, err error) {
	if err = r.conn.Take(&fetchMeta).Error; err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, err
		}
		return &models.FetchMeta{GoCPEDictRevision: config.Revision, SchemaVersion: models.LatestSchemaVersion}, nil
	}

	return fetchMeta, nil
}

// UpsertFetchMeta upsert FetchMeta to Database
func (r *RDBDriver) UpsertFetchMeta(fetchMeta *models.FetchMeta) error {
	fetchMeta.GoCPEDictRevision = config.Revision
	fetchMeta.SchemaVersion = models.LatestSchemaVersion
	return r.conn.Save(fetchMeta).Error
}

// GetVendorProducts : GetVendorProducts
func (r *RDBDriver) GetVendorProducts() (vendorProducts []string, err error) {
	var results []struct {
		Vendor  string
		Product string
	}

	// TODO Is there a better way to use distinct with GORM? Needing
	// explicit column names seems like an antipattern for an orm.
	err = r.conn.Model(&models.CategorizedCpe{}).Distinct("vendor", "product").Find(&results).Error
	if err != nil && errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("Failed to select results. err: %s", err)
	}

	for _, vp := range results {
		vendorProducts = append(vendorProducts, fmt.Sprintf("%s::%s", vp.Vendor, vp.Product))
	}
	return
}

// GetCpesByVendorProduct : GetCpesByVendorProduct
func (r *RDBDriver) GetCpesByVendorProduct(vendor, product string) ([]string, []string, error) {
	results := []models.CategorizedCpe{}
	err := r.conn.Distinct("cpe_uri", "deprecated").Find(&results, "vendor LIKE ? and product LIKE ?", vendor, product).Error
	if err != nil && errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil, fmt.Errorf("Failed to select results. err: %s", err)
	}
	cpeURIs, deprecated := []string{}, []string{}
	for _, r := range results {
		if r.Deprecated {
			deprecated = append(deprecated, r.CpeURI)
		} else {
			cpeURIs = append(cpeURIs, r.CpeURI)
		}
	}
	return cpeURIs, deprecated, nil
}

// InsertCpes inserts Cpe Information into DB
func (r *RDBDriver) InsertCpes(fetchType models.FetchType, cpes []models.CategorizedCpe) error {
	return r.deleteAndInsertCpes(r.conn, fetchType, cpes)
}

func (r *RDBDriver) deleteAndInsertCpes(conn *gorm.DB, fetchType models.FetchType, cpes []models.CategorizedCpe) (err error) {
	bar := pb.StartNew(len(cpes))
	tx := conn.Begin()
	defer func() {
		if err != nil {
			tx.Rollback()
			return
		}
		tx.Commit()
	}()

	// Delete all old records
	oldIDs := []int64{}
	result := tx.Model(models.CategorizedCpe{}).Select("id").Where("fetch_type = ?", fetchType).Find(&oldIDs)
	if result.Error != nil && !errors.Is(result.Error, gorm.ErrRecordNotFound) {
		return xerrors.Errorf("Failed to select old defs: %w", result.Error)
	}

	if result.RowsAffected > 0 {
		for idx := range chunkSlice(len(oldIDs), 10000) {
			if err := tx.Where("id IN ?", oldIDs[idx.From:idx.To]).Delete(&models.CategorizedCpe{}).Error; err != nil {
				return xerrors.Errorf("Failed to delete: %w", err)
			}
		}
	}

	for idx := range chunkSlice(len(cpes), 2000) {
		if err := tx.Create(cpes[idx.From:idx.To]).Error; err != nil {
			return xerrors.Errorf("Failed to insert. err: %w", err)
		}
		bar.Add(idx.To - idx.From)
	}
	bar.Finish()

	return nil
}

// IsDeprecated : IsDeprecated
func (r *RDBDriver) IsDeprecated(cpeURI string) (bool, error) {
	// not implemented yet
	return false, nil
}

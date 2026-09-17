package db

import (
	"fmt"

	"github.com/rs/zerolog/log"
	"gorm.io/gorm"
)

type MigrationVersion struct {
	ID      uint `gorm:"primaryKey"`
	Version uint
}

func applyMigrations(dbInfo *databaseInfo) error {
	switch dbInfo.Type {
	case SQLite, PostgreSQL, MySQL:
		return applyAllMigrations(dbInfo.Type)
	default:
		return fmt.Errorf("unknown database type: %s", dbInfo.Type)
	}
}

func applyAllMigrations(dbType databaseType) error {
	if err := db.AutoMigrate(&MigrationVersion{}); err != nil {
		log.Fatal().Err(err).Msg("Error creating migration version table")
		return err
	}

	var currentVersion MigrationVersion
	db.First(&currentVersion)

	migrations := []struct {
		Version uint
		DBTypes []databaseType
		Func    func() error
	}{
		{1, []databaseType{SQLite}, v1_modifyConstraintToSSHKeys},
		{2, []databaseType{SQLite}, v2_lowercaseEmails},
		{3, nil, v3_normalizedColumns},
		{4, nil, v4_uniqueGistUserUrlIndex},
	}

	for _, m := range migrations {
		if m.Version <= currentVersion.Version {
			continue
		}

		if len(m.DBTypes) > 0 {
			applicable := false
			for _, t := range m.DBTypes {
				if t == dbType {
					applicable = true
					break
				}
			}
			if !applicable {
				currentVersion.Version = m.Version
				db.Save(&currentVersion)
				continue
			}
		}

		tx := db.Begin()
		if err := tx.Error; err != nil {
			log.Fatal().Err(err).Msg("Error starting transaction")
			return err
		}

		if err := m.Func(); err != nil {
			tx.Rollback()
			log.Fatal().Err(err).Msg(fmt.Sprintf("Error applying migration %d:", m.Version))
			return err
		}

		if err := tx.Commit().Error; err != nil {
			log.Fatal().Err(err).Msg(fmt.Sprintf("Error committing migration %d:", m.Version))
			return err
		}

		currentVersion.Version = m.Version
		db.Save(&currentVersion)
		log.Info().Msg(fmt.Sprintf("Migration %d applied successfully", m.Version))
	}

	return nil
}

func v1_modifyConstraintToSSHKeys() error {
	createSQL := `
	CREATE TABLE ssh_keys_temp (
		id integer primary key,
		title text,
		content text,
		sha text,
		created_at integer,
		last_used_at integer,
		user_id integer
		constraint fk_users_ssh_keys references users(id) on update cascade on delete cascade
	);
	`

	if err := db.Exec(createSQL).Error; err != nil {
		return err
	}

	copySQL := `INSERT INTO ssh_keys_temp SELECT * FROM ssh_keys;`
	if err := db.Exec(copySQL).Error; err != nil {
		return err
	}

	dropSQL := `DROP TABLE ssh_keys;`
	if err := db.Exec(dropSQL).Error; err != nil {
		return err
	}

	renameSQL := `ALTER TABLE ssh_keys_temp RENAME TO ssh_keys;`
	return db.Exec(renameSQL).Error
}

func v2_lowercaseEmails() error {
	copySQL := `UPDATE users SET email = lower(email);`
	return db.Exec(copySQL).Error
}

func v3_normalizedColumns() error {
	if err := db.Model(&User{}).Where("username_normalized = '' OR username_normalized IS NULL").
		Updates(map[string]interface{}{"username_normalized": gorm.Expr("LOWER(username)")}).Error; err != nil {
		return err
	}
	return db.Model(&Gist{}).Where("url_normalized = '' OR url_normalized IS NULL").
		Updates(map[string]interface{}{"url_normalized": gorm.Expr("LOWER(url)")}).Error
}

func v4_uniqueGistUserUrlIndex() error {
	var gists []Gist
	if err := db.Order("id").Find(&gists).Error; err != nil {
		return err
	}

	seen := make(map[uint]map[string]bool)

	for _, gist := range gists {
		if seen[gist.UserID] == nil {
			seen[gist.UserID] = make(map[string]bool)
		}

		url := gist.URL
		if !seen[gist.UserID][url] {
			seen[gist.UserID][url] = true
			continue
		}

		for suffix := 1; ; suffix++ {
			candidate := fmt.Sprintf("%s-%d", url, suffix)
			if seen[gist.UserID][candidate] {
				continue
			}

			if err := db.Model(&Gist{}).
				Where("id = ?", gist.ID).
				Updates(map[string]interface{}{
					"url":            candidate,
					"url_normalized": gorm.Expr("LOWER(?)", candidate),
				}).Error; err != nil {
				return err
			}

			seen[gist.UserID][candidate] = true
			break
		}
	}

	return db.Migrator().CreateIndex(&Gist{}, "idx_gists_user_url")
}

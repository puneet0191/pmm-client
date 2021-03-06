/*
	Copyright (c) 2016, Percona LLC and/or its affiliates. All rights reserved.

	This program is free software: you can redistribute it and/or modify
	it under the terms of the GNU Affero General Public License as published by
	the Free Software Foundation, either version 3 of the License, or
	(at your option) any later version.

	This program is distributed in the hope that it will be useful,
	but WITHOUT ANY WARRANTY; without even the implied warranty of
	MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
	GNU Affero General Public License for more details.

	You should have received a copy of the GNU Affero General Public License
	along with this program.  If not, see <http://www.gnu.org/licenses/>
*/

package pmm

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"regexp"
	"strings"
	"time"

	"github.com/Masterminds/semver"
	"github.com/percona/go-mysql/dsn"
)

// MySQLFlags are MySQL specific flags.
type MySQLFlags struct {
	DefaultsFile string
	User         string
	Password     string
	Host         string
	Port         string
	Socket       string

	CreateUser         bool
	CreateUserPassword string
	MaxUserConn        uint16
	Force              bool
}

// MySQLInfo describes running MySQL instance.
type MySQLInfo struct {
	Hostname string
	Port     string
	Distro   string
	Version  string
	DSN      string
	SafeDSN  string
}

// DetectMySQL detect MySQL, create user if needed, return DSN and MySQL info strings.
func (a *Admin) DetectMySQL(ctx context.Context, mf MySQLFlags) (*MySQLInfo, error) {
	// Check for invalid mix of flags.
	if mf.Socket != "" && mf.Host != "" {
		return nil, errors.New("flags --socket and --host are mutually exclusive")
	}
	if mf.Socket != "" && mf.Port != "" {
		return nil, errors.New("flags --socket and --port are mutually exclusive")
	}
	if !mf.CreateUser && mf.CreateUserPassword != "" {
		return nil, errors.New("flag --create-user-password should be used along with --create-user")
	}

	userDSN := dsn.DSN{
		DefaultsFile: mf.DefaultsFile,
		Username:     mf.User,
		Password:     mf.Password,
		Hostname:     mf.Host,
		Port:         mf.Port,
		Socket:       mf.Socket,
		Params:       []string{dsn.ParseTimeParam, dsn.TimezoneParam, dsn.LocationParam},
	}
	// Populate defaults to DSN for missing options.
	userDSN, err := userDSN.AutoDetect(ctx)
	if err != nil && err != dsn.ErrNoSocket {
		err = fmt.Errorf("problem with MySQL auto-detection: %s", err)
		return nil, err
	}

	db, err := sql.Open("mysql", userDSN.String())
	if err != nil {
		return nil, err
	}
	defer db.Close()

	// Test access using detected credentials and stored password.
	accessOK := false
	if a.Config.MySQLPassword != "" {
		pmmDSN := userDSN
		pmmDSN.Username = "pmm"
		pmmDSN.Password = a.Config.MySQLPassword
		if err := testConnection(ctx, pmmDSN.String()); err == nil {
			//fmt.Println("Using stored credentials, DSN is", pmmDSN.String())
			accessOK = true
			userDSN = pmmDSN
			// Not setting this into db connection as it will never have GRANT
			// in case we want to create a new user below.
		}
	}

	// If the above fails, test MySQL access simply using detected credentials.
	if !accessOK {
		if err := testConnection(ctx, userDSN.String()); err != nil {
			err = fmt.Errorf("Cannot connect to MySQL: %s\n\n%s\n%s", err,
				"Verify that MySQL user exists and has the correct privileges.",
				"Use additional flags --user, --password, --host, --port, --socket if needed.")
			return nil, err
		}
	}

	// At this point, we verified the MySQL access, so no need to handle SQL errors below
	// if our queries are predictably good.

	// Create a new MySQL user.
	if mf.CreateUser {
		userDSN, err = createMySQLUser(ctx, db, userDSN, mf)
		if err != nil {
			return nil, err
		}

		// Store generated password.
		a.Config.MySQLPassword = userDSN.Password
		a.writeConfig()
	}

	// Get MySQL variables.
	mi := getMysqlInfo(ctx, db)
	mi.DSN = userDSN.String()
	mi.SafeDSN = SanitizeDSN(userDSN.String())

	return mi, nil
}

func createMySQLUser(ctx context.Context, db *sql.DB, userDSN dsn.DSN, mf MySQLFlags) (dsn.DSN, error) {
	// New DSN has same host:port or socket, but different user and pass.
	userDSN.Username = "pmm"
	if mf.CreateUserPassword != "" {
		userDSN.Password = mf.CreateUserPassword
	} else {
		userDSN.Password = generatePassword(20)
	}

	hosts := []string{"%"}
	if userDSN.Socket != "" || userDSN.Hostname == "localhost" {
		hosts = []string{"localhost", "127.0.0.1"}
	} else if userDSN.Hostname == "127.0.0.1" {
		hosts = []string{"127.0.0.1"}
	}

	if !mf.Force {
		if err := mysqlCheck(ctx, db, hosts); err != nil {
			return dsn.DSN{}, err
		}
	}

	// Create a new MySQL user with the necessary privs.
	grants, err := makeGrants(ctx, db, userDSN, hosts, mf.MaxUserConn)
	if err != nil {
		return dsn.DSN{}, err
	}
	for _, grant := range grants {
		if _, err := db.Exec(grant); err != nil {
			err = fmt.Errorf("Problem creating a new MySQL user. Failed to execute %s: %s\n\n%s",
				grant, err, "Verify that connecting MySQL user has GRANT privilege.")
			return dsn.DSN{}, err
		}
	}

	// Verify new MySQL user works. If this fails, the new DSN or grant statements are wrong.
	if err := testConnection(ctx, userDSN.String()); err != nil {
		err = fmt.Errorf("Problem creating a new MySQL user. Insufficient privileges: %s", err)
		return dsn.DSN{}, err
	}

	return userDSN, nil
}

func mysqlCheck(ctx context.Context, db *sql.DB, hosts []string) error {
	var (
		errMsg []string
		varVal string
	)

	// Check for read_only.
	if db.QueryRowContext(ctx, "SELECT @@read_only").Scan(&varVal); varVal == "1" {
		errMsg = append(errMsg, "* You are trying to write on read-only MySQL host.")
	}

	// Check for slave.
	if slaveStatusRows, err := db.QueryContext(ctx, "SHOW SLAVE STATUS"); err == nil {
		if slaveStatusRows.Next() {
			errMsg = append(errMsg, "* You are trying to write on MySQL replication slave.")
		}
	}

	// Check if user exists.
	for _, host := range hosts {
		if rows, err := db.QueryContext(ctx, fmt.Sprintf("SHOW GRANTS FOR 'pmm'@'%s'", host)); err == nil {
			// MariaDB requires to check .Next() because err is always nil even user doesn't exist %)
			if !rows.Next() {
				continue
			}
			if host == "%" {
				host = "%%"
			}
			errMsg = append(errMsg, fmt.Sprintf("* MySQL user pmm@%s already exists. %s", host,
				"Try without --create-user flag using the default credentials or specify the existing `pmm` user ones."))
			break
		}
	}

	if len(errMsg) > 0 {
		errMsg = append([]string{"Problem creating a new MySQL user:", ""}, errMsg...)
		errMsg = append(errMsg, "", "If you think the above is okay to proceed, you can use --force flag.")
		return errors.New(strings.Join(errMsg, "\n"))
	}

	return nil
}

func makeGrants(ctx context.Context, db *sql.DB, dsn dsn.DSN, hosts []string, conn uint16) ([]string, error) {
	var grants []string
	for _, host := range hosts {
		// Privileges:
		// PROCESS - for mysqld_exporter to get all processes from `SHOW PROCESSLIST`
		// REPLICATION CLIENT - for mysqld_exporter to run `SHOW BINARY LOGS`
		// RELOAD - for qan-agent to run `FLUSH SLOW LOGS`
		// SUPER - for qan-agent to set global variables (not clear it is still required)
		// Grants for performance_schema - for qan-agent to manage query digest tables.
		atLeastMySQL57, err := versionConstraint(ctx, db, ">= 5.7.0")
		if err != nil {
			return nil, err
		}
		if atLeastMySQL57 {
			exists, err := userExists(ctx, db, dsn.Username, host)
			if err != nil {
				return nil, err
			}
			if exists {
				grants = append(grants,
					fmt.Sprintf("ALTER USER '%s'@'%s' IDENTIFIED BY '%s' WITH MAX_USER_CONNECTIONS %d",
						dsn.Username, host, dsn.Password, conn),
				)
			} else {
				grants = append(grants,
					fmt.Sprintf("CREATE USER '%s'@'%s' IDENTIFIED BY '%s' WITH MAX_USER_CONNECTIONS %d",
						dsn.Username, host, dsn.Password, conn),
				)
			}
			grants = append(grants,
				fmt.Sprintf("GRANT SELECT, PROCESS, REPLICATION CLIENT, RELOAD, SUPER ON *.* TO '%s'@'%s'",
					dsn.Username, host),
			)
		} else {
			grants = append(grants,
				fmt.Sprintf("GRANT SELECT, PROCESS, REPLICATION CLIENT, RELOAD, SUPER ON *.* TO '%s'@'%s' IDENTIFIED BY '%s' WITH MAX_USER_CONNECTIONS %d",
					dsn.Username, host, dsn.Password, conn),
			)
		}
		grants = append(grants,
			fmt.Sprintf("GRANT UPDATE, DELETE, DROP ON `performance_schema`.* TO '%s'@'%s'", dsn.Username, host),
		)
	}

	return grants, nil
}

func userExists(ctx context.Context, db *sql.DB, user, host string) (bool, error) {
	count := 0
	err := db.QueryRowContext(ctx, "SELECT 1 FROM mysql.user WHERE user=? AND host=?", user, host).Scan(&count)
	switch {
	case err == sql.ErrNoRows:
		return false, nil
	case err != nil:
		return false, err
	case count == 0:
		// Shouldn't happen but just in case, if we get row and 0 value then user doesn't exists.
		return false, nil
	}
	return true, nil
}

func testConnection(ctx context.Context, dsn string) error {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return err
	}
	defer db.Close()

	if err = db.PingContext(ctx); err != nil {
		return err
	}

	return nil
}

func getMysqlInfo(ctx context.Context, db *sql.DB) *MySQLInfo {
	mi := &MySQLInfo{}
	db.QueryRowContext(ctx, "SELECT @@hostname, @@port, @@version_comment, @@version").Scan(&mi.Hostname, &mi.Port, &mi.Distro, &mi.Version)
	return mi
}

// generatePassword generate password to satisfy MySQL 5.7 default password policy.
func generatePassword(size int) string {
	rand.Seed(time.Now().UnixNano())
	required := []string{
		"abcdefghijklmnopqrstuvwxyz", "ABCDEFGHIJKLMNOPQRSTUVWXYZ", "0123456789", "_,;-",
	}
	var b []rune

	for _, source := range required {
		rsource := []rune(source)
		for i := 0; i < int(size/len(required))+1; i++ {
			b = append(b, rsource[rand.Intn(len(rsource))])
		}
	}
	// Scramble.
	for range b {
		pos1 := rand.Intn(len(b))
		pos2 := rand.Intn(len(b))
		a := b[pos1]
		b[pos1] = b[pos2]
		b[pos2] = a
	}
	return string(b)[:size]
}

// versionConstraint checks if version fits given constraint.
func versionConstraint(ctx context.Context, db *sql.DB, constraint string) (bool, error) {
	version := sql.NullString{}
	err := db.QueryRowContext(ctx, "SELECT @@GLOBAL.version").Scan(&version)
	if err != nil {
		return false, err
	}

	// Strip everything after the first dash
	re := regexp.MustCompile("-.*$")
	version.String = re.ReplaceAllString(version.String, "")
	v, err := semver.NewVersion(version.String)
	if err != nil {
		return false, err
	}

	constraints, err := semver.NewConstraint(constraint)
	if err != nil {
		return false, err
	}
	return constraints.Check(v), nil
}

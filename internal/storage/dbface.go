package storage

import (
    _ "github.com/go-sql-driver/mysql"
    "database/sql"
    "github.com/google/gopacket"
    "github.com/shopspring/decimal"
    log "github.com/sirupsen/logrus"
    "net"
    "errors"
    "time"
)

type UseEvent struct {
    UserAddress gopacket.Endpoint
    BytesUp int64
    BytesDown int64
}

type UserStatus struct {
    UserAddress        gopacket.Endpoint
    CurrentDataBalance int64
    PriorDataBalance   int64
    CurrencyBalance    decimal.Decimal
}

func LogUsage(db *sql.DB, event UseEvent) (UserStatus, error) {
    ip := net.ParseIP(event.UserAddress.String())
    if ip == nil {
        log.WithField("Endpoint", event.UserAddress).Error("Unable to parse user IP")
    }

    // Attempt to commit an update 3 times, barring other more serious errors.
    for i := 0; i < 3; i++ {
        trx, err := db.Begin()
        if err != nil {
            log.WithField("UseEvent", event).WithError(err).Error("Unable to begin transaction")
            return UserStatus{}, err
        }

        var imsi int64
        err = trx.QueryRow("select imsi from static_ips where ip=?", ip.String()).Scan(&imsi)
        if err != nil {
            log.WithField("ip", ip).WithError(err).Error("Unable to lookup imsi's static ip address")
            // TODO(matt9j) Consider deferring the rollback?
            trx.Rollback()
            return UserStatus{}, err
        }

        var (
            rawDown     int64
            rawUp       int64
            dataBalance int64
            balance     decimal.Decimal
            bridged     bool
            enabled     bool
        )

        err = trx.QueryRow(
            "SELECT raw_down, raw_up, data_balance, balance, bridged, enabled FROM customers WHERE imsi=? ",
            imsi).Scan(&rawDown, &rawUp, &dataBalance, &balance, &bridged, &enabled)
        if err != nil {
            log.WithField("imsi", imsi).WithError(err).Error("Unable to lookup customer data")
            trx.Rollback()
            return UserStatus{}, err
        }

        // Business logic accounting for the event.
        // TODO(matt9j) Continuing to log the raw up/down for the interval to match the python implementation, but
        // should now no longer be necessary.
        rawDown = event.BytesDown
        rawUp = event.BytesUp
        priorDataBalance := dataBalance
        dataBalance -= event.BytesUp
        dataBalance -= event.BytesDown

        _, err = trx.Exec(
            "UPDATE customers SET raw_down=?, raw_up=?, data_balance=?, enabled=?, bridged=? WHERE imsi=?",
            rawDown, rawUp, dataBalance, enabled, bridged, imsi)
        if err != nil {
            log.WithField("imsi", imsi).WithError(err).Error("Unable to execute update customer data")
            trx.Rollback()
            return UserStatus{}, err
        }

        err = trx.Commit()
        if err != nil {
            log.WithField("Attempt", i).WithField("imsi", imsi).WithError(err).Warn("Unable to commit")
        } else {
            return UserStatus{event.UserAddress, dataBalance, priorDataBalance, balance}, err
        }
    }
    log.WithField("User", event.UserAddress).Error("Giving up committing billing update!")
    return UserStatus{}, errors.New("data loss: unable to commit")
}

func UpdateBridgedState(db *sql.DB, userIP net.IP, bridged bool) error {
    // Attempt to commit an update 3 times, barring other more serious errors.
    for i := 0; i < 3; i++ {
        trx, err := db.Begin()
        if err != nil {
            log.WithError(err).Error("Unable to begin bridge update transaction")
            return err
        }

        var imsi int64
        err = trx.QueryRow("select imsi from static_ips where ip=?", userIP.String()).Scan(&imsi)
        if err != nil {
            log.WithField("ip", userIP).WithError(err).Error("Unable to lookup imsi's static ip address")
            // TODO(matt9j) Consider deferring the rollback?
            trx.Rollback()
            return err
        }

        _, err = trx.Exec("UPDATE customers SET bridged=? WHERE imsi=?", bridged, imsi)
        if err != nil {
            log.WithField("imsi", imsi).WithError(err).Error("Unable to execute update customer bridged data")
            trx.Rollback()
            return err
        }

        err = trx.Commit()
        if err != nil {
            log.WithField("Attempt", i).WithField("imsi", imsi).WithError(err).Warn("Unable to commit")
        }
    }

    return nil
}

func LogFlow(db *sql.DB, start time.Time, stop time.Time, flow gopacket.Flow, hostA string, hostB string, bytesAB int, bytesBA int) {
    // TODO(matt9j) Lookup the correct hostname to host number mapping and insert if necessary.

    _, err := db.Exec("INSERT INTO flowlogs VALUE (?, ?, ?, ?, ?, ?, ?, ?)",
        start, stop, flow.Src().Raw(), flow.Dst().Raw(), 0, 0, bytesAB, bytesBA)
    if err != nil {
        // TODO(matt9j) Log the flow event itself once one is defined.
        log.WithError(err).Error("Unable to commit a flow log!!!")
    }
}

type UserBridgedState struct {
    Addr    net.IP
    Bridged bool
}

func QueryGlobalBridgedState(db *sql.DB) ([]UserBridgedState) {
    rows, err := db.Query("SELECT ip, bridged FROM customers, static_ips WHERE customers.imsi=static_ips.imsi AND enabled=1")
    if err != nil {
        log.WithError(err).Error("Unable to query initial bridged state")
    }
    defer rows.Close()

    var ipString string
    var bridged bool

    globalState := make([]UserBridgedState, 0)

    for rows.Next() {
        if err := rows.Scan(&ipString, &bridged); err != nil {
            log.WithError(err).Error("Unable to scan bridged state")
        }

        addr := net.ParseIP(ipString)
        if addr == nil {
            log.WithField("String", ipString).Error("Unable to parse string to IP")
        }
        globalState = append(globalState, UserBridgedState{addr, bridged})
    }
    if err = rows.Err(); err != nil {
        log.WithError(err).Error("Error encountered when reading bridged state rows")
    }

    return globalState
}

func QueryToppedUpCustomers(db * sql.DB) ([]net.IP) {
    // Topped up customers are customers that have data balance but are not bridged!
    rows, err := db.Query("SELECT ip FROM customers, static_ips WHERE customers.imsi=static_ips.imsi AND enabled=1 AND data_balance>0 AND bridged=0")
    if err != nil {
        log.WithError(err).Error("Unable to query topped up customers")
    }
    defer rows.Close()

    var ipString string

    toppedUpUsers := make([]net.IP, 0)

    for rows.Next() {
        if err := rows.Scan(&ipString); err != nil {
            log.WithError(err).Error("Unable to scan topped up ip")
        }

        addr := net.ParseIP(ipString)
        if addr == nil {
            log.WithField("String", ipString).Error("Unable to parse string to IP")
        }
        toppedUpUsers = append(toppedUpUsers, addr)
    }
    if err = rows.Err(); err != nil {
        log.WithError(err).Error("Error encountered when reading topped up user rows")
    }

    return toppedUpUsers
}
// Copyright 2021 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package sessioninit

import (
	"context"
	"fmt"
	"unsafe"

	"github.com/cockroachdb/cockroach/pkg/kv"
	"github.com/cockroachdb/cockroach/pkg/security"
	"github.com/cockroachdb/cockroach/pkg/settings"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descs"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlutil"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/mon"
	"github.com/cockroachdb/cockroach/pkg/util/stop"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil/singleflight"
	"github.com/cockroachdb/logtags"
)

// CacheEnabledSettingName is the name of the CacheEnabled cluster setting.
var CacheEnabledSettingName = "server.authentication_cache.enabled"

// CacheEnabled is a cluster setting that determines if the
// sessioninit.Cache and associated logic is enabled.
var CacheEnabled = settings.RegisterBoolSetting(
	settings.TenantWritable,
	CacheEnabledSettingName,
	"enables a cache used during authentication to avoid lookups to system tables "+
		"when retrieving per-user authentication-related information",
	true,
).WithPublic()

// Cache is a shared cache for hashed passwords and other information used
// during user authentication and session initialization.
type Cache struct {
	syncutil.Mutex
	usersTableVersion          descpb.DescriptorVersion
	roleOptionsTableVersion    descpb.DescriptorVersion
	dbRoleSettingsTableVersion descpb.DescriptorVersion
	boundAccount               mon.BoundAccount
	// authInfoCache is a mapping from username to AuthInfo.
	authInfoCache map[security.SQLUsername]AuthInfo
	// settingsCache is a mapping from (dbID, username) to default settings.
	settingsCache map[SettingsCacheKey][]string
	// populateCacheGroup is used to ensure that there is at most one in-flight
	// request for populating each cache entry.
	populateCacheGroup singleflight.Group
	stopper            *stop.Stopper
}

// AuthInfo contains data that is used to perform an authentication attempt.
type AuthInfo struct {
	// UserExists is set to true if the user has a row in system.users.
	UserExists bool
	// CanLoginSQL is set to false if the user has the NOLOGIN or NOSQLLOGIN role option.
	CanLoginSQL bool
	// CanLoginDBConsole is set to false if the user has NOLOGIN role option.
	CanLoginDBConsole bool
	// HashedPassword is the hashed password and can be nil.
	HashedPassword security.PasswordHash
	// ValidUntil is the VALID UNTIL role option.
	ValidUntil *tree.DTimestamp
}

// SettingsCacheKey is the key used for the settingsCache.
type SettingsCacheKey struct {
	DatabaseID descpb.ID
	Username   security.SQLUsername
}

// SettingsCacheEntry represents an entry in the settingsCache. It is
// used so that the entries can be returned in a stable order.
type SettingsCacheEntry struct {
	SettingsCacheKey
	Settings []string
}

// NewCache initializes a new sessioninit.Cache.
func NewCache(account mon.BoundAccount, stopper *stop.Stopper) *Cache {
	return &Cache{
		boundAccount: account,
		stopper:      stopper,
	}
}

// GetAuthInfo consults the sessioninit.Cache and returns the AuthInfo for the
// provided username and databaseName. If the information is not in the cache,
// or if the underlying tables have changed since the cache was populated,
// then the readFromSystemTables callback is used to load new data.
func (a *Cache) GetAuthInfo(
	ctx context.Context,
	settings *cluster.Settings,
	ie sqlutil.InternalExecutor,
	db *kv.DB,
	f *descs.CollectionFactory,
	username security.SQLUsername,
	readFromSystemTables func(
		ctx context.Context,
		txn *kv.Txn,
		ie sqlutil.InternalExecutor,
		username security.SQLUsername,
	) (AuthInfo, error),
) (aInfo AuthInfo, err error) {
	if !CacheEnabled.Get(&settings.SV) {
		return readFromSystemTables(ctx, nil /* txn */, ie, username)
	}
	err = f.Txn(ctx, ie, db, func(
		ctx context.Context, txn *kv.Txn, descriptors *descs.Collection,
	) error {
		_, usersTableDesc, err := descriptors.GetImmutableTableByName(
			ctx,
			txn,
			UsersTableName,
			tree.ObjectLookupFlagsWithRequired(),
		)
		if err != nil {
			return err
		}
		_, roleOptionsTableDesc, err := descriptors.GetImmutableTableByName(
			ctx,
			txn,
			RoleOptionsTableName,
			tree.ObjectLookupFlagsWithRequired(),
		)
		if err != nil {
			return err
		}

		// If the underlying table versions are not committed, stop and avoid
		// trying to cache anything.
		if usersTableDesc.IsUncommittedVersion() ||
			roleOptionsTableDesc.IsUncommittedVersion() {
			aInfo, err = readFromSystemTables(ctx, txn, ie, username)
			return err
		}
		usersTableVersion := usersTableDesc.GetVersion()
		roleOptionsTableVersion := roleOptionsTableDesc.GetVersion()

		// Check version and maybe clear cache while holding the mutex.
		var found bool
		aInfo, found = a.readAuthInfoFromCache(ctx, usersTableVersion, roleOptionsTableVersion, username)

		if found {
			return nil
		}

		// Lookup the data outside the lock. There will be at most one
		// request in-flight for each user. The user and role_options table
		// versions are also part of the request key so that we don't read data
		// from an old version of either table.
		val, err := a.loadCacheValue(
			ctx, fmt.Sprintf("authinfo-%s-%d-%d", username.Normalized(), usersTableVersion, roleOptionsTableVersion),
			func(loadCtx context.Context) (interface{}, error) {
				return readFromSystemTables(loadCtx, txn, ie, username)
			})
		if err != nil {
			return err
		}
		aInfo = val.(AuthInfo)

		// Write data back to the cache if the table version hasn't changed.
		a.maybeWriteAuthInfoBackToCache(
			ctx,
			usersTableVersion,
			roleOptionsTableVersion,
			aInfo,
			username,
		)
		return nil
	})
	return aInfo, err
}

func (a *Cache) readAuthInfoFromCache(
	ctx context.Context,
	usersTableVersion descpb.DescriptorVersion,
	roleOptionsTableVersion descpb.DescriptorVersion,
	username security.SQLUsername,
) (AuthInfo, bool) {
	a.Lock()
	defer a.Unlock()
	// We don't need to check dbRoleSettingsTableVersion here, so pass in the
	// one we already have.
	isEligibleForCache := a.clearCacheIfStale(ctx, usersTableVersion, roleOptionsTableVersion, a.dbRoleSettingsTableVersion)
	if !isEligibleForCache {
		return AuthInfo{}, false
	}
	ai, foundAuthInfo := a.authInfoCache[username]
	return ai, foundAuthInfo
}

// loadCacheValue loads the value for the given requestKey using the provided
// function. It ensures that there is only at most one in-flight request for
// each key at any time.
func (a *Cache) loadCacheValue(
	ctx context.Context, requestKey string, fn func(loadCtx context.Context) (interface{}, error),
) (interface{}, error) {
	ch, _ := a.populateCacheGroup.DoChan(requestKey, func() (interface{}, error) {
		// Use a different context to fetch, so that it isn't possible for
		// one query to timeout and cause all the goroutines that are waiting
		// to get a timeout error.
		loadCtx, cancel := a.stopper.WithCancelOnQuiesce(
			logtags.WithTags(context.Background(), logtags.FromContext(ctx)),
		)
		defer cancel()
		return fn(loadCtx)
	})
	select {
	case res := <-ch:
		if res.Err != nil {
			return AuthInfo{}, res.Err
		}
		return res.Val, nil
	case <-ctx.Done():
		return AuthInfo{}, ctx.Err()
	}
}

// maybeWriteAuthInfoBackToCache tries to put the fetched AuthInfo into the
// authInfoCache, and returns true if it succeeded. If the underlying system
// tables have been modified since they were read, the authInfoCache is not
// updated.
func (a *Cache) maybeWriteAuthInfoBackToCache(
	ctx context.Context,
	usersTableVersion descpb.DescriptorVersion,
	roleOptionsTableVersion descpb.DescriptorVersion,
	aInfo AuthInfo,
	username security.SQLUsername,
) bool {
	a.Lock()
	defer a.Unlock()
	// Table versions have changed while we were looking: don't cache the data.
	if a.usersTableVersion != usersTableVersion || a.roleOptionsTableVersion != roleOptionsTableVersion {
		return false
	}
	// Table version remains the same: update map, unlock, return.
	const sizeOfUsername = int(unsafe.Sizeof(security.SQLUsername{}))
	const sizeOfAuthInfo = int(unsafe.Sizeof(AuthInfo{}))
	const sizeOfTimestamp = int(unsafe.Sizeof(tree.DTimestamp{}))

	hpSize := 0
	if aInfo.HashedPassword != nil {
		hpSize = aInfo.HashedPassword.Size()
	}

	sizeOfEntry := sizeOfUsername + len(username.Normalized()) +
		sizeOfAuthInfo + hpSize +
		sizeOfTimestamp
	if err := a.boundAccount.Grow(ctx, int64(sizeOfEntry)); err != nil {
		// If there is no memory available to cache the entry, we can still
		// proceed with authentication so that users are not locked out of
		// the database.
		log.Ops.Warningf(ctx, "no memory available to cache authentication info: %v", err)
	} else {
		a.authInfoCache[username] = aInfo
	}
	return true
}

// GetDefaultSettings consults the sessioninit.Cache and returns the list of
// SettingsCacheEntry for the provided username and databaseName. If the
// information is not in the cache, or if the underlying tables have changed
// since the cache was populated, then the readFromSystemTables callback is
// used to load new data.
func (a *Cache) GetDefaultSettings(
	ctx context.Context,
	settings *cluster.Settings,
	ie sqlutil.InternalExecutor,
	db *kv.DB,
	f *descs.CollectionFactory,
	username security.SQLUsername,
	databaseName string,
	readFromSystemTables func(
		ctx context.Context,
		txn *kv.Txn,
		ie sqlutil.InternalExecutor,
		username security.SQLUsername,
		databaseID descpb.ID,
	) ([]SettingsCacheEntry, error),
) (settingsEntries []SettingsCacheEntry, err error) {
	err = f.Txn(ctx, ie, db, func(
		ctx context.Context, txn *kv.Txn, descriptors *descs.Collection,
	) error {
		_, dbRoleSettingsTableDesc, err := descriptors.GetImmutableTableByName(
			ctx,
			txn,
			DatabaseRoleSettingsTableName,
			tree.ObjectLookupFlagsWithRequired(),
		)
		if err != nil {
			return err
		}
		databaseID := descpb.ID(0)
		if databaseName != "" {
			dbDesc, err := descriptors.GetImmutableDatabaseByName(ctx, txn, databaseName, tree.DatabaseLookupFlags{})
			if err != nil {
				return err
			}
			// If dbDesc is nil, the database name was not valid, but that should
			// not cause a login-preventing error.
			if dbDesc != nil {
				databaseID = dbDesc.GetID()
			}
		}

		// If the underlying table versions are not committed or if the cache is
		// disabled, stop and avoid trying to cache anything.
		// We can't check if the cache is disabled earlier, since we always need to
		// start the `CollectionFactory.Txn()` regardless in order to look up the
		// database descriptor ID.
		if dbRoleSettingsTableDesc.IsUncommittedVersion() || !CacheEnabled.Get(&settings.SV) {
			settingsEntries, err = readFromSystemTables(
				ctx,
				txn,
				ie,
				username,
				databaseID,
			)
			return err
		}
		dbRoleSettingsTableVersion := dbRoleSettingsTableDesc.GetVersion()

		// Check version and maybe clear cache while holding the mutex.
		var found bool
		settingsEntries, found = a.readDefaultSettingsFromCache(ctx, dbRoleSettingsTableVersion, username, databaseID)

		if found {
			return nil
		}

		// Lookup the data outside the lock. There will be at most one request
		// in-flight for each user+database. The db_role_settings table version is
		// also part of the request key so that we don't read data from an old
		// version of the table.
		val, err := a.loadCacheValue(
			ctx, fmt.Sprintf("defaultsettings-%s-%d-%d", username.Normalized(), databaseID, dbRoleSettingsTableVersion),
			func(loadCtx context.Context) (interface{}, error) {
				return readFromSystemTables(loadCtx, txn, ie, username, databaseID)
			},
		)
		if err != nil {
			return err
		}
		settingsEntries = val.([]SettingsCacheEntry)

		// Write the fetched data back to the cache if the table version hasn't
		// changed.
		a.maybeWriteDefaultSettingsBackToCache(
			ctx,
			dbRoleSettingsTableVersion,
			settingsEntries,
		)
		return nil
	})
	return settingsEntries, err
}

func (a *Cache) readDefaultSettingsFromCache(
	ctx context.Context,
	dbRoleSettingsTableVersion descpb.DescriptorVersion,
	username security.SQLUsername,
	databaseID descpb.ID,
) ([]SettingsCacheEntry, bool) {
	a.Lock()
	defer a.Unlock()
	// We don't need to check usersTableVersion or roleOptionsTableVersion here,
	// so pass in the values we already have.
	isEligibleForCache := a.clearCacheIfStale(
		ctx, a.usersTableVersion, a.roleOptionsTableVersion, dbRoleSettingsTableVersion,
	)
	if !isEligibleForCache {
		return nil, false
	}
	foundAllDefaultSettings := true
	var sEntries []SettingsCacheEntry
	// Search through the cache for the settings entries we need. Since we look up
	// multiple entries in the cache, the same setting might appear multiple
	// times. Note that GenerateSettingsCacheKeys goes in order of precedence,
	// so the order of the returned []SettingsCacheEntry is important and the
	// caller must take care not to apply a setting if it has already appeared
	// earlier in the list.
	for _, k := range GenerateSettingsCacheKeys(databaseID, username) {
		s, ok := a.settingsCache[k]
		if !ok {
			foundAllDefaultSettings = false
			break
		}
		sEntries = append(sEntries, SettingsCacheEntry{k, s})
	}
	return sEntries, foundAllDefaultSettings
}

// maybeWriteDefaultSettingsBackToCache tries to put the fetched SettingsCacheEntry
// list into the settingsCache, and returns true if it succeeded. If the
// underlying system tables have been modified since they were read, the
// settingsCache is not updated.
func (a *Cache) maybeWriteDefaultSettingsBackToCache(
	ctx context.Context,
	dbRoleSettingsTableVersion descpb.DescriptorVersion,
	settingsEntries []SettingsCacheEntry,
) bool {
	a.Lock()
	defer a.Unlock()
	// Table version has changed while we were looking: don't cache the data.
	if a.dbRoleSettingsTableVersion != dbRoleSettingsTableVersion {
		return false
	}

	// Table version remains the same: update map, unlock, return.
	const sizeOfSettingsCacheEntry = int(unsafe.Sizeof(SettingsCacheEntry{}))
	sizeOfSettings := 0
	for _, sEntry := range settingsEntries {
		if _, ok := a.settingsCache[sEntry.SettingsCacheKey]; ok {
			// Avoid double-counting memory if a key is already in the cache.
			continue
		}
		sizeOfSettings += sizeOfSettingsCacheEntry
		sizeOfSettings += len(sEntry.SettingsCacheKey.Username.Normalized())
		for _, s := range sEntry.Settings {
			sizeOfSettings += len(s)
		}
	}
	if err := a.boundAccount.Grow(ctx, int64(sizeOfSettings)); err != nil {
		// If there is no memory available to cache the entry, we can still
		// proceed with authentication so that users are not locked out of
		// the database.
		log.Ops.Warningf(ctx, "no memory available to cache authentication info: %v", err)
	} else {
		for _, sEntry := range settingsEntries {
			// Avoid re-storing an existing key.
			if _, ok := a.settingsCache[sEntry.SettingsCacheKey]; !ok {
				a.settingsCache[sEntry.SettingsCacheKey] = sEntry.Settings
			}
		}
	}
	return true
}

// clearCacheIfStale compares the cached table versions to the current table
// versions. If the cached versions are older, the cache is cleared. If the
// cached versions are newer, then false is returned to indicate that the
// cached data should not be used.
func (a *Cache) clearCacheIfStale(
	ctx context.Context,
	usersTableVersion descpb.DescriptorVersion,
	roleOptionsTableVersion descpb.DescriptorVersion,
	dbRoleSettingsTableVersion descpb.DescriptorVersion,
) (isEligibleForCache bool) {
	if a.usersTableVersion < usersTableVersion ||
		a.roleOptionsTableVersion < roleOptionsTableVersion ||
		a.dbRoleSettingsTableVersion < dbRoleSettingsTableVersion {
		// If the cache is based on old table versions, then update versions and
		// drop the map.
		a.usersTableVersion = usersTableVersion
		a.roleOptionsTableVersion = roleOptionsTableVersion
		a.dbRoleSettingsTableVersion = dbRoleSettingsTableVersion
		a.authInfoCache = make(map[security.SQLUsername]AuthInfo)
		a.settingsCache = make(map[SettingsCacheKey][]string)
		a.boundAccount.Empty(ctx)
	} else if a.usersTableVersion > usersTableVersion ||
		a.roleOptionsTableVersion > roleOptionsTableVersion ||
		a.dbRoleSettingsTableVersion > dbRoleSettingsTableVersion {
		// If the cache is based on newer table versions, then this transaction
		// should not use the cached data.
		return false
	}
	return true
}

// GenerateSettingsCacheKeys returns a slice of all the SettingsCacheKey
// that are relevant for the given databaseID and username. The slice is
// ordered in descending order of precedence.
func GenerateSettingsCacheKeys(
	databaseID descpb.ID, username security.SQLUsername,
) []SettingsCacheKey {
	return []SettingsCacheKey{
		{
			DatabaseID: databaseID,
			Username:   username,
		},
		{
			DatabaseID: defaultDatabaseID,
			Username:   username,
		},
		{
			DatabaseID: databaseID,
			Username:   defaultUsername,
		},
		{
			DatabaseID: defaultDatabaseID,
			Username:   defaultUsername,
		},
	}
}

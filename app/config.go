// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

package app

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/md5"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"time"

	"github.com/pkg/errors"

	"github.com/mattermost/mattermost-server/v6/config"
	"github.com/mattermost/mattermost-server/v6/model"
	"github.com/mattermost/mattermost-server/v6/shared/mail"
	"github.com/mattermost/mattermost-server/v6/shared/mlog"
	"github.com/mattermost/mattermost-server/v6/utils"
)

const (
	ErrorTermsOfServiceNoRowsFound = "app.terms_of_service.get.no_rows.app_error"
)

// configWrapper is an adapter struct that only exposes the
// config related functionality to be passed down to other products.
type configWrapper struct {
	srv *Server
	*config.Store
}

func (w *configWrapper) Name() ServiceKey {
	return ConfigKey
}

func (w *configWrapper) Config() *model.Config {
	return w.Store.Get()
}

func (w *configWrapper) AddConfigListener(listener func(*model.Config, *model.Config)) string {
	return w.Store.AddListener(listener)
}

func (w *configWrapper) RemoveConfigListener(id string) {
	w.Store.RemoveListener(id)
}

func (w *configWrapper) UpdateConfig(f func(*model.Config)) {
	if w.Store.IsReadOnly() {
		return
	}
	old := w.Config()
	updated := old.Clone()
	f(updated)
	if _, _, err := w.Store.Set(updated); err != nil {
		mlog.Error("Failed to update config", mlog.Err(err))
	}
}

func (w *configWrapper) SaveConfig(newCfg *model.Config, sendConfigChangeClusterMessage bool) (*model.Config, *model.Config, *model.AppError) {
	oldCfg, newCfg, err := w.Store.Set(newCfg)
	if errors.Cause(err) == config.ErrReadOnlyConfiguration {
		return nil, nil, model.NewAppError("saveConfig", "ent.cluster.save_config.error", nil, err.Error(), http.StatusForbidden)
	} else if err != nil {
		return nil, nil, model.NewAppError("saveConfig", "app.save_config.app_error", nil, err.Error(), http.StatusInternalServerError)
	}

	if w.srv.startMetrics && *w.Config().MetricsSettings.Enable {
		if w.srv.Metrics != nil {
			w.srv.Metrics.Register()
		}
		w.srv.SetupMetricsServer()
	} else {
		w.srv.StopMetricsServer()
	}

	if w.srv.Cluster != nil {
		err := w.srv.Cluster.ConfigChanged(w.Store.RemoveEnvironmentOverrides(oldCfg),
			w.Store.RemoveEnvironmentOverrides(newCfg), sendConfigChangeClusterMessage)
		if err != nil {
			return nil, nil, err
		}
	}

	return oldCfg, newCfg, nil
}

func (w *configWrapper) ReloadConfig() error {
	if err := w.Store.Load(); err != nil {
		return err
	}
	return nil
}

func (s *Server) Config() *model.Config {
	return s.configStore.Config()
}

func (s *Server) ConfigStore() *configWrapper {
	return s.configStore
}

func (a *App) Config() *model.Config {
	return a.ch.cfgSvc.Config()
}

func (s *Server) EnvironmentConfig(filter func(reflect.StructField) bool) map[string]interface{} {
	return s.configStore.GetEnvironmentOverridesWithFilter(filter)
}

func (a *App) EnvironmentConfig(filter func(reflect.StructField) bool) map[string]interface{} {
	return a.Srv().EnvironmentConfig(filter)
}

func (s *Server) UpdateConfig(f func(*model.Config)) {
	s.configStore.UpdateConfig(f)
}

func (a *App) UpdateConfig(f func(*model.Config)) {
	a.Srv().UpdateConfig(f)
}

func (s *Server) ReloadConfig() error {
	return s.configStore.ReloadConfig()
}

func (a *App) ReloadConfig() error {
	return a.Srv().ReloadConfig()
}

func (a *App) ClientConfig() map[string]string {
	return a.ch.clientConfig.Load().(map[string]string)
}

func (a *App) ClientConfigHash() string {
	return a.ch.ClientConfigHash()
}

func (a *App) LimitedClientConfig() map[string]string {
	return a.ch.limitedClientConfig.Load().(map[string]string)
}

// Registers a function with a given listener to be called when the config is reloaded and may have changed. The function
// will be called with two arguments: the old config and the new config. AddConfigListener returns a unique ID
// for the listener that can later be used to remove it.
func (s *Server) AddConfigListener(listener func(*model.Config, *model.Config)) string {
	return s.configStore.AddConfigListener(listener)
}

func (a *App) AddConfigListener(listener func(*model.Config, *model.Config)) string {
	return a.Srv().AddConfigListener(listener)
}

// Removes a listener function by the unique ID returned when AddConfigListener was called
func (s *Server) RemoveConfigListener(id string) {
	s.configStore.RemoveConfigListener(id)
}

func (a *App) RemoveConfigListener(id string) {
	a.Srv().RemoveConfigListener(id)
}

// ensurePostActionCookieSecret ensures that the key for encrypting PostActionCookie exists
// and future calls to PostActionCookieSecret will always return a valid key, same on all
// servers in the cluster
func (ch *Channels) ensurePostActionCookieSecret() error {
	if ch.postActionCookieSecret != nil {
		return nil
	}

	var secret *model.SystemPostActionCookieSecret

	value, err := ch.srv.Store.System().GetByName(model.SystemPostActionCookieSecretKey)
	if err == nil {
		if err := json.Unmarshal([]byte(value.Value), &secret); err != nil {
			return err
		}
	}

	// If we don't already have a key, try to generate one.
	if secret == nil {
		newSecret := &model.SystemPostActionCookieSecret{
			Secret: make([]byte, 32),
		}
		_, err := rand.Reader.Read(newSecret.Secret)
		if err != nil {
			return err
		}

		system := &model.System{
			Name: model.SystemPostActionCookieSecretKey,
		}
		v, err := json.Marshal(newSecret)
		if err != nil {
			return err
		}
		system.Value = string(v)
		// If we were able to save the key, use it, otherwise log the error.
		if err = ch.srv.Store.System().Save(system); err != nil {
			mlog.Warn("Failed to save PostActionCookieSecret", mlog.Err(err))
		} else {
			secret = newSecret
		}
	}

	// If we weren't able to save a new key above, another server must have beat us to it. Get the
	// key from the database, and if that fails, error out.
	if secret == nil {
		value, err := ch.srv.Store.System().GetByName(model.SystemPostActionCookieSecretKey)
		if err != nil {
			return err
		}

		if err := json.Unmarshal([]byte(value.Value), &secret); err != nil {
			return err
		}
	}

	ch.postActionCookieSecret = secret.Secret
	return nil
}

// ensureAsymmetricSigningKey ensures that an asymmetric signing key exists and future calls to
// AsymmetricSigningKey will always return a valid signing key.
func (ch *Channels) ensureAsymmetricSigningKey() error {
	if ch.AsymmetricSigningKey() != nil {
		return nil
	}

	var key *model.SystemAsymmetricSigningKey

	value, err := ch.srv.Store.System().GetByName(model.SystemAsymmetricSigningKeyKey)
	if err == nil {
		if err := json.Unmarshal([]byte(value.Value), &key); err != nil {
			return err
		}
	}

	// If we don't already have a key, try to generate one.
	if key == nil {
		newECDSAKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return err
		}
		newKey := &model.SystemAsymmetricSigningKey{
			ECDSAKey: &model.SystemECDSAKey{
				Curve: "P-256",
				X:     newECDSAKey.X,
				Y:     newECDSAKey.Y,
				D:     newECDSAKey.D,
			},
		}
		system := &model.System{
			Name: model.SystemAsymmetricSigningKeyKey,
		}
		v, err := json.Marshal(newKey)
		if err != nil {
			return err
		}
		system.Value = string(v)
		// If we were able to save the key, use it, otherwise log the error.
		if err = ch.srv.Store.System().Save(system); err != nil {
			mlog.Warn("Failed to save AsymmetricSigningKey", mlog.Err(err))
		} else {
			key = newKey
		}
	}

	// If we weren't able to save a new key above, another server must have beat us to it. Get the
	// key from the database, and if that fails, error out.
	if key == nil {
		value, err := ch.srv.Store.System().GetByName(model.SystemAsymmetricSigningKeyKey)
		if err != nil {
			return err
		}

		if err := json.Unmarshal([]byte(value.Value), &key); err != nil {
			return err
		}
	}

	var curve elliptic.Curve
	switch key.ECDSAKey.Curve {
	case "P-256":
		curve = elliptic.P256()
	default:
		return fmt.Errorf("unknown curve: " + key.ECDSAKey.Curve)
	}
	ch.asymmetricSigningKey.Store(&ecdsa.PrivateKey{
		PublicKey: ecdsa.PublicKey{
			Curve: curve,
			X:     key.ECDSAKey.X,
			Y:     key.ECDSAKey.Y,
		},
		D: key.ECDSAKey.D,
	})
	ch.regenerateClientConfig()
	return nil
}

func (s *Server) ensureInstallationDate() error {
	_, appErr := s.getSystemInstallDate()
	if appErr == nil {
		return nil
	}

	installDate, nErr := s.Store.User().InferSystemInstallDate()
	var installationDate int64
	if nErr == nil && installDate > 0 {
		installationDate = installDate
	} else {
		installationDate = utils.MillisFromTime(time.Now())
	}

	if err := s.Store.System().SaveOrUpdate(&model.System{
		Name:  model.SystemInstallationDateKey,
		Value: strconv.FormatInt(installationDate, 10),
	}); err != nil {
		return err
	}
	return nil
}

func (s *Server) ensureFirstServerRunTimestamp() error {
	_, appErr := s.getFirstServerRunTimestamp()
	if appErr == nil {
		return nil
	}

	if err := s.Store.System().SaveOrUpdate(&model.System{
		Name:  model.SystemFirstServerRunTimestampKey,
		Value: strconv.FormatInt(utils.MillisFromTime(time.Now()), 10),
	}); err != nil {
		return err
	}
	return nil
}

// AsymmetricSigningKey will return a private key that can be used for asymmetric signing.
func (ch *Channels) AsymmetricSigningKey() *ecdsa.PrivateKey {
	if key := ch.asymmetricSigningKey.Load(); key != nil {
		return key.(*ecdsa.PrivateKey)
	}
	return nil
}

func (a *App) AsymmetricSigningKey() *ecdsa.PrivateKey {
	return a.ch.AsymmetricSigningKey()
}

func (ch *Channels) PostActionCookieSecret() []byte {
	return ch.postActionCookieSecret
}

func (a *App) PostActionCookieSecret() []byte {
	return a.ch.PostActionCookieSecret()
}

func (ch *Channels) regenerateClientConfig() {
	clientConfig := config.GenerateClientConfig(ch.cfgSvc.Config(), ch.srv.TelemetryId(), ch.srv.License())
	limitedClientConfig := config.GenerateLimitedClientConfig(ch.cfgSvc.Config(), ch.srv.TelemetryId(), ch.srv.License())

	if clientConfig["EnableCustomTermsOfService"] == "true" {
		termsOfService, err := ch.srv.Store.TermsOfService().GetLatest(true)
		if err != nil {
			mlog.Err(err)
		} else {
			clientConfig["CustomTermsOfServiceId"] = termsOfService.Id
			limitedClientConfig["CustomTermsOfServiceId"] = termsOfService.Id
		}
	}

	if key := ch.AsymmetricSigningKey(); key != nil {
		der, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
		clientConfig["AsymmetricSigningPublicKey"] = base64.StdEncoding.EncodeToString(der)
		limitedClientConfig["AsymmetricSigningPublicKey"] = base64.StdEncoding.EncodeToString(der)
	}

	clientConfigJSON, _ := json.Marshal(clientConfig)
	ch.clientConfig.Store(clientConfig)
	ch.limitedClientConfig.Store(limitedClientConfig)
	ch.clientConfigHash.Store(fmt.Sprintf("%x", md5.Sum(clientConfigJSON)))
}

func (a *App) GetCookieDomain() string {
	if *a.Config().ServiceSettings.AllowCookiesForSubdomains {
		if siteURL, err := url.Parse(*a.Config().ServiceSettings.SiteURL); err == nil {
			return siteURL.Hostname()
		}
	}
	return ""
}

func (a *App) GetSiteURL() string {
	return *a.Config().ServiceSettings.SiteURL
}

// ClientConfigWithComputed gets the configuration in a format suitable for sending to the client.
func (a *App) ClientConfigWithComputed() map[string]string {
	respCfg := map[string]string{}
	for k, v := range a.ch.clientConfig.Load().(map[string]string) {
		respCfg[k] = v
	}

	// These properties are not configurable, but nevertheless represent configuration expected
	// by the client.
	respCfg["NoAccounts"] = strconv.FormatBool(a.ch.srv.userService.IsFirstUserAccount())
	respCfg["MaxPostSize"] = strconv.Itoa(a.ch.srv.MaxPostSize())
	respCfg["UpgradedFromTE"] = strconv.FormatBool(a.ch.srv.isUpgradedFromTE())
	respCfg["InstallationDate"] = ""
	if installationDate, err := a.ch.srv.getSystemInstallDate(); err == nil {
		respCfg["InstallationDate"] = strconv.FormatInt(installationDate, 10)
	}

	return respCfg
}

// LimitedClientConfigWithComputed gets the configuration in a format suitable for sending to the client.
func (a *App) LimitedClientConfigWithComputed() map[string]string {
	respCfg := map[string]string{}
	for k, v := range a.LimitedClientConfig() {
		respCfg[k] = v
	}

	// These properties are not configurable, but nevertheless represent configuration expected
	// by the client.
	respCfg["NoAccounts"] = strconv.FormatBool(a.IsFirstUserAccount())

	return respCfg
}

// GetConfigFile proxies access to the given configuration file to the underlying config store.
func (a *App) GetConfigFile(name string) ([]byte, error) {
	data, err := a.Srv().configStore.GetFile(name)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get config file %s", name)
	}

	return data, nil
}

// GetSanitizedConfig gets the configuration for a system admin without any secrets.
func (a *App) GetSanitizedConfig() *model.Config {
	cfg := a.Config().Clone()
	cfg.Sanitize()

	return cfg
}

// GetEnvironmentConfig returns a map of configuration keys whose values have been overridden by an environment variable.
// If filter is not nil and returns false for a struct field, that field will be omitted.
func (a *App) GetEnvironmentConfig(filter func(reflect.StructField) bool) map[string]interface{} {
	return a.EnvironmentConfig(filter)
}

// SaveConfig replaces the active configuration, optionally notifying cluster peers.
// It returns both the previous and current configs.
func (s *Server) SaveConfig(newCfg *model.Config, sendConfigChangeClusterMessage bool) (*model.Config, *model.Config, *model.AppError) {
	return s.configStore.SaveConfig(newCfg, sendConfigChangeClusterMessage)
}

// SaveConfig replaces the active configuration, optionally notifying cluster peers.
func (a *App) SaveConfig(newCfg *model.Config, sendConfigChangeClusterMessage bool) (*model.Config, *model.Config, *model.AppError) {
	return a.Srv().SaveConfig(newCfg, sendConfigChangeClusterMessage)
}

func (a *App) HandleMessageExportConfig(cfg *model.Config, appCfg *model.Config) {
	// If the Message Export feature has been toggled in the System Console, rewrite the ExportFromTimestamp field to an
	// appropriate value. The rewriting occurs here to ensure it doesn't affect values written to the config file
	// directly and not through the System Console UI.
	if *cfg.MessageExportSettings.EnableExport != *appCfg.MessageExportSettings.EnableExport {
		if *cfg.MessageExportSettings.EnableExport && *cfg.MessageExportSettings.ExportFromTimestamp == int64(0) {
			// When the feature is toggled on, use the current timestamp as the start time for future exports.
			cfg.MessageExportSettings.ExportFromTimestamp = model.NewInt64(model.GetMillis())
		} else if !*cfg.MessageExportSettings.EnableExport {
			// When the feature is disabled, reset the timestamp so that the timestamp will be set if
			// the feature is re-enabled from the System Console in future.
			cfg.MessageExportSettings.ExportFromTimestamp = model.NewInt64(0)
		}
	}
}

func (s *Server) MailServiceConfig() *mail.SMTPConfig {
	emailSettings := s.Config().EmailSettings
	hostname := utils.GetHostnameFromSiteURL(*s.Config().ServiceSettings.SiteURL)
	cfg := mail.SMTPConfig{
		Hostname:                          hostname,
		ConnectionSecurity:                *emailSettings.ConnectionSecurity,
		SkipServerCertificateVerification: *emailSettings.SkipServerCertificateVerification,
		ServerName:                        *emailSettings.SMTPServer,
		Server:                            *emailSettings.SMTPServer,
		Port:                              *emailSettings.SMTPPort,
		ServerTimeout:                     *emailSettings.SMTPServerTimeout,
		Username:                          *emailSettings.SMTPUsername,
		Password:                          *emailSettings.SMTPPassword,
		EnableSMTPAuth:                    *emailSettings.EnableSMTPAuth,
		SendEmailNotifications:            *emailSettings.SendEmailNotifications,
		FeedbackName:                      *emailSettings.FeedbackName,
		FeedbackEmail:                     *emailSettings.FeedbackEmail,
		ReplyToAddress:                    *emailSettings.ReplyToAddress,
	}
	return &cfg
}

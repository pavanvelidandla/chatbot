// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package utils

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"

	"github.com/fsnotify/fsnotify"
	"github.com/pkg/errors"
	"github.com/spf13/viper"

	"net/http"

	"github.com/mattermost/mattermost-server/einterfaces"
	"github.com/mattermost/mattermost-server/mlog"
	"github.com/mattermost/mattermost-server/model"
	"github.com/mattermost/mattermost-server/utils/jsonutils"
)

const (
	LOG_ROTATE_SIZE = 10000
	LOG_FILENAME    = "mattermost.log"
)

// FindConfigFile attempts to find an existing configuration file. fileName can be an absolute or
// relative path or name such as "/opt/mattermost/config.json" or simply "config.json". An empty
// string is returned if no configuration is found.
func FindConfigFile(fileName string) (path string) {
	if filepath.IsAbs(fileName) {
		if _, err := os.Stat(fileName); err == nil {
			return fileName
		}
	} else {
		for _, dir := range []string{"./config", "../config", "../../config", "."} {
			path, _ := filepath.Abs(filepath.Join(dir, fileName))
			if _, err := os.Stat(path); err == nil {
				return path
			}
		}
	}
	return ""
}

// FindDir looks for the given directory in nearby ancestors, falling back to `./` if not found.
func FindDir(dir string) (string, bool) {
	for _, parent := range []string{".", "..", "../.."} {
		foundDir, err := filepath.Abs(filepath.Join(parent, dir))
		if err != nil {
			continue
		} else if _, err := os.Stat(foundDir); err == nil {
			return foundDir, true
		}
	}
	return "./", false
}

func MloggerConfigFromLoggerConfig(s *model.LogSettings) *mlog.LoggerConfiguration {
	return &mlog.LoggerConfiguration{
		EnableConsole: s.EnableConsole,
		ConsoleJson:   *s.ConsoleJson,
		ConsoleLevel:  strings.ToLower(s.ConsoleLevel),
		EnableFile:    s.EnableFile,
		FileJson:      *s.FileJson,
		FileLevel:     strings.ToLower(s.FileLevel),
		FileLocation:  GetLogFileLocation(s.FileLocation),
	}
}

// DON'T USE THIS Modify the level on the app logger
func DisableDebugLogForTest() {
	mlog.GloballyDisableDebugLogForTest()
}

// DON'T USE THIS Modify the level on the app logger
func EnableDebugLogForTest() {
	mlog.GloballyEnableDebugLogForTest()
}

func GetLogFileLocation(fileLocation string) string {
	if fileLocation == "" {
		fileLocation, _ = FindDir("logs")
	}

	return filepath.Join(fileLocation, LOG_FILENAME)
}

func SaveConfig(fileName string, config *model.Config) *model.AppError {
	b, err := json.MarshalIndent(config, "", "    ")
	if err != nil {
		return model.NewAppError("SaveConfig", "utils.config.save_config.saving.app_error",
			map[string]interface{}{"Filename": fileName}, err.Error(), http.StatusBadRequest)
	}

	err = ioutil.WriteFile(fileName, b, 0644)
	if err != nil {
		return model.NewAppError("SaveConfig", "utils.config.save_config.saving.app_error",
			map[string]interface{}{"Filename": fileName}, err.Error(), http.StatusInternalServerError)
	}

	return nil
}

type ConfigWatcher struct {
	watcher *fsnotify.Watcher
	close   chan struct{}
	closed  chan struct{}
}

func NewConfigWatcher(cfgFileName string, f func()) (*ConfigWatcher, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, errors.Wrapf(err, "failed to create config watcher for file: "+cfgFileName)
	}

	configFile := filepath.Clean(cfgFileName)
	configDir, _ := filepath.Split(configFile)
	watcher.Add(configDir)

	ret := &ConfigWatcher{
		watcher: watcher,
		close:   make(chan struct{}),
		closed:  make(chan struct{}),
	}

	go func() {
		defer close(ret.closed)
		defer watcher.Close()

		for {
			select {
			case event := <-watcher.Events:
				// we only care about the config file
				if filepath.Clean(event.Name) == configFile {
					if event.Op&fsnotify.Write == fsnotify.Write || event.Op&fsnotify.Create == fsnotify.Create {
						mlog.Info(fmt.Sprintf("Config file watcher detected a change reloading %v", cfgFileName))

						if _, _, configReadErr := ReadConfigFile(cfgFileName, true); configReadErr == nil {
							f()
						} else {
							mlog.Error(fmt.Sprintf("Failed to read while watching config file at %v with err=%v", cfgFileName, configReadErr.Error()))
						}
					}
				}
			case err := <-watcher.Errors:
				mlog.Error(fmt.Sprintf("Failed while watching config file at %v with err=%v", cfgFileName, err.Error()))
			case <-ret.close:
				return
			}
		}
	}()

	return ret, nil
}

func (w *ConfigWatcher) Close() {
	close(w.close)
	<-w.closed
}

// ReadConfig reads and parses the given configuration.
func ReadConfig(r io.Reader, allowEnvironmentOverrides bool) (*model.Config, map[string]interface{}, error) {
	// Pre-flight check the syntax of the configuration file to improve error messaging.
	configData, err := ioutil.ReadAll(r)
	if err != nil {
		return nil, nil, err
	} else {
		var rawConfig interface{}
		if err := json.Unmarshal(configData, &rawConfig); err != nil {
			return nil, nil, jsonutils.HumanizeJsonError(err, configData)
		}
	}

	v := newViper(allowEnvironmentOverrides)
	if err := v.ReadConfig(bytes.NewReader(configData)); err != nil {
		return nil, nil, err
	}

	var config model.Config
	unmarshalErr := v.Unmarshal(&config)
	if unmarshalErr == nil {
		// https://github.com/spf13/viper/issues/324
		// https://github.com/spf13/viper/issues/348
		config.PluginSettings = model.PluginSettings{}
		unmarshalErr = v.UnmarshalKey("pluginsettings", &config.PluginSettings)
	}

	envConfig := v.EnvSettings()

	var envErr error
	if envConfig, envErr = fixEnvSettingsCase(envConfig); envErr != nil {
		return nil, nil, envErr
	}

	return &config, envConfig, unmarshalErr
}

func newViper(allowEnvironmentOverrides bool) *viper.Viper {
	v := viper.New()

	v.SetConfigType("json")

	if allowEnvironmentOverrides {
		v.SetEnvPrefix("mm")
		v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
		v.AutomaticEnv()
	}

	// Set zeroed defaults for all the config settings so that Viper knows what environment variables
	// it needs to be looking for. The correct defaults will later be applied using Config.SetDefaults.
	defaults := getDefaultsFromStruct(model.Config{})

	for key, value := range defaults {
		v.SetDefault(key, value)
	}

	return v
}

func getDefaultsFromStruct(s interface{}) map[string]interface{} {
	return flattenStructToMap(structToMap(reflect.TypeOf(s)))
}

// Converts a struct type into a nested map with keys matching the struct's fields and values
// matching the zeroed value of the corresponding field.
func structToMap(t reflect.Type) (out map[string]interface{}) {
	defer func() {
		if r := recover(); r != nil {
			mlog.Error(fmt.Sprintf("Panicked in structToMap. This should never happen. %v", r))
		}
	}()

	if t.Kind() != reflect.Struct {
		// Should never hit this, but this will prevent a panic if that does happen somehow
		return nil
	}

	out = map[string]interface{}{}

	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)

		var value interface{}

		switch field.Type.Kind() {
		case reflect.Struct:
			value = structToMap(field.Type)
		case reflect.Ptr:
			indirectType := field.Type.Elem()

			if indirectType.Kind() == reflect.Struct {
				// Follow pointers to structs since we need to define defaults for their fields
				value = structToMap(indirectType)
			} else {
				value = nil
			}
		default:
			value = reflect.Zero(field.Type).Interface()
		}

		out[field.Name] = value
	}

	return
}

// Flattens a nested map so that the result is a single map with keys corresponding to the
// path through the original map. For example,
// {
//     "a": {
//         "b": 1
//     },
//     "c": "sea"
// }
// would flatten to
// {
//     "a.b": 1,
//     "c": "sea"
// }
func flattenStructToMap(in map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{})

	for key, value := range in {
		if valueAsMap, ok := value.(map[string]interface{}); ok {
			sub := flattenStructToMap(valueAsMap)

			for subKey, subValue := range sub {
				out[key+"."+subKey] = subValue
			}
		} else {
			out[key] = value
		}
	}

	return out
}

// Fixes the case of the environment variables sent back from Viper since Viper stores
// everything as lower case.
func fixEnvSettingsCase(in map[string]interface{}) (out map[string]interface{}, err error) {
	defer func() {
		if r := recover(); r != nil {
			mlog.Error(fmt.Sprintf("Panicked in fixEnvSettingsCase. This should never happen. %v", r))
			out = in
		}
	}()

	var fixCase func(map[string]interface{}, reflect.Type) map[string]interface{}
	fixCase = func(in map[string]interface{}, t reflect.Type) map[string]interface{} {
		if t.Kind() != reflect.Struct {
			// Should never hit this, but this will prevent a panic if that does happen somehow
			return nil
		}

		out := make(map[string]interface{}, len(in))

		for i := 0; i < t.NumField(); i++ {
			field := t.Field(i)

			key := field.Name
			if value, ok := in[strings.ToLower(key)]; ok {
				if valueAsMap, ok := value.(map[string]interface{}); ok {
					out[key] = fixCase(valueAsMap, field.Type)
				} else {
					out[key] = value
				}
			}
		}

		return out
	}

	out = fixCase(in, reflect.TypeOf(model.Config{}))

	return
}

// ReadConfigFile reads and parses the configuration at the given file path.
func ReadConfigFile(path string, allowEnvironmentOverrides bool) (*model.Config, map[string]interface{}, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	return ReadConfig(f, allowEnvironmentOverrides)
}

// EnsureConfigFile will attempt to locate a config file with the given name. If it does not exist,
// it will attempt to locate a default config file, and copy it to a file named fileName in the same
// directory. In either case, the config file path is returned.
func EnsureConfigFile(fileName string) (string, error) {
	if configFile := FindConfigFile(fileName); configFile != "" {
		return configFile, nil
	}
	if defaultPath := FindConfigFile("default.json"); defaultPath != "" {
		destPath := filepath.Join(filepath.Dir(defaultPath), fileName)
		src, err := os.Open(defaultPath)
		if err != nil {
			return "", err
		}
		defer src.Close()
		dest, err := os.OpenFile(destPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600)
		if err != nil {
			return "", err
		}
		defer dest.Close()
		if _, err := io.Copy(dest, src); err == nil {
			return destPath, nil
		}
	}
	return "", fmt.Errorf("no config file found")
}

// LoadConfig will try to search around for the corresponding config file.  It will search
// /tmp/fileName then attempt ./config/fileName, then ../config/fileName and last it will look at
// fileName.
func LoadConfig(fileName string) (*model.Config, string, map[string]interface{}, *model.AppError) {
	var configPath string

	if fileName != filepath.Base(fileName) {
		configPath = fileName
	} else {
		if path, err := EnsureConfigFile(fileName); err != nil {
			appErr := model.NewAppError("LoadConfig", "utils.config.load_config.opening.panic", map[string]interface{}{"Filename": fileName, "Error": err.Error()}, "", 0)
			return nil, "", nil, appErr
		} else {
			configPath = path
		}
	}

	config, envConfig, err := ReadConfigFile(configPath, true)
	if err != nil {
		appErr := model.NewAppError("LoadConfig", "utils.config.load_config.decoding.panic", map[string]interface{}{"Filename": fileName, "Error": err.Error()}, "", 0)
		return nil, "", nil, appErr
	}

	needSave := len(config.SqlSettings.AtRestEncryptKey) == 0 || len(*config.FileSettings.PublicLinkSalt) == 0 ||
		len(config.EmailSettings.InviteSalt) == 0

	config.SetDefaults()

	if err := config.IsValid(); err != nil {
		return nil, "", nil, err
	}

	if needSave {
		if err := SaveConfig(configPath, config); err != nil {
			mlog.Warn(err.Error())
		}
	}

	if err := ValidateLocales(config); err != nil {
		if err := SaveConfig(configPath, config); err != nil {
			mlog.Warn(err.Error())
		}
	}

	if *config.FileSettings.DriverName == model.IMAGE_DRIVER_LOCAL {
		dir := config.FileSettings.Directory
		if len(dir) > 0 && dir[len(dir)-1:] != "/" {
			config.FileSettings.Directory += "/"
		}
	}

	return config, configPath, envConfig, nil
}

func GenerateClientConfig(c *model.Config, diagnosticId string, license *model.License) map[string]string {
	props := make(map[string]string)

	props["Version"] = model.CurrentVersion
	props["BuildNumber"] = model.BuildNumber
	props["BuildDate"] = model.BuildDate
	props["BuildHash"] = model.BuildHash
	props["BuildHashEnterprise"] = model.BuildHashEnterprise
	props["BuildEnterpriseReady"] = model.BuildEnterpriseReady

	props["SiteURL"] = strings.TrimRight(*c.ServiceSettings.SiteURL, "/")
	props["WebsocketURL"] = strings.TrimRight(*c.ServiceSettings.WebsocketURL, "/")
	props["SiteName"] = c.TeamSettings.SiteName
	props["EnableTeamCreation"] = strconv.FormatBool(*c.TeamSettings.EnableTeamCreation)
	props["EnableAPIv3"] = strconv.FormatBool(*c.ServiceSettings.EnableAPIv3)
	props["EnableUserCreation"] = strconv.FormatBool(c.TeamSettings.EnableUserCreation)
	props["EnableOpenServer"] = strconv.FormatBool(*c.TeamSettings.EnableOpenServer)
	props["RestrictDirectMessage"] = *c.TeamSettings.RestrictDirectMessage
	props["RestrictTeamInvite"] = *c.TeamSettings.RestrictTeamInvite
	props["RestrictPublicChannelCreation"] = *c.TeamSettings.RestrictPublicChannelCreation
	props["RestrictPrivateChannelCreation"] = *c.TeamSettings.RestrictPrivateChannelCreation
	props["RestrictPublicChannelManagement"] = *c.TeamSettings.RestrictPublicChannelManagement
	props["RestrictPrivateChannelManagement"] = *c.TeamSettings.RestrictPrivateChannelManagement
	props["RestrictPublicChannelDeletion"] = *c.TeamSettings.RestrictPublicChannelDeletion
	props["RestrictPrivateChannelDeletion"] = *c.TeamSettings.RestrictPrivateChannelDeletion
	props["RestrictPrivateChannelManageMembers"] = *c.TeamSettings.RestrictPrivateChannelManageMembers
	props["EnableXToLeaveChannelsFromLHS"] = strconv.FormatBool(*c.TeamSettings.EnableXToLeaveChannelsFromLHS)
	props["TeammateNameDisplay"] = *c.TeamSettings.TeammateNameDisplay
	props["ExperimentalPrimaryTeam"] = *c.TeamSettings.ExperimentalPrimaryTeam

	props["AndroidLatestVersion"] = c.ClientRequirements.AndroidLatestVersion
	props["AndroidMinVersion"] = c.ClientRequirements.AndroidMinVersion
	props["DesktopLatestVersion"] = c.ClientRequirements.DesktopLatestVersion
	props["DesktopMinVersion"] = c.ClientRequirements.DesktopMinVersion
	props["IosLatestVersion"] = c.ClientRequirements.IosLatestVersion
	props["IosMinVersion"] = c.ClientRequirements.IosMinVersion

	props["EnableOAuthServiceProvider"] = strconv.FormatBool(c.ServiceSettings.EnableOAuthServiceProvider)
	props["GoogleDeveloperKey"] = c.ServiceSettings.GoogleDeveloperKey
	props["EnableIncomingWebhooks"] = strconv.FormatBool(c.ServiceSettings.EnableIncomingWebhooks)
	props["EnableOutgoingWebhooks"] = strconv.FormatBool(c.ServiceSettings.EnableOutgoingWebhooks)
	props["EnableCommands"] = strconv.FormatBool(*c.ServiceSettings.EnableCommands)
	props["EnableOnlyAdminIntegrations"] = strconv.FormatBool(*c.ServiceSettings.EnableOnlyAdminIntegrations)
	props["EnablePostUsernameOverride"] = strconv.FormatBool(c.ServiceSettings.EnablePostUsernameOverride)
	props["EnablePostIconOverride"] = strconv.FormatBool(c.ServiceSettings.EnablePostIconOverride)
	props["EnableUserAccessTokens"] = strconv.FormatBool(*c.ServiceSettings.EnableUserAccessTokens)
	props["EnableLinkPreviews"] = strconv.FormatBool(*c.ServiceSettings.EnableLinkPreviews)
	props["EnableTesting"] = strconv.FormatBool(c.ServiceSettings.EnableTesting)
	props["EnableDeveloper"] = strconv.FormatBool(*c.ServiceSettings.EnableDeveloper)
	props["EnableDiagnostics"] = strconv.FormatBool(*c.LogSettings.EnableDiagnostics)
	props["RestrictPostDelete"] = *c.ServiceSettings.RestrictPostDelete
	props["AllowEditPost"] = *c.ServiceSettings.AllowEditPost
	props["PostEditTimeLimit"] = fmt.Sprintf("%v", *c.ServiceSettings.PostEditTimeLimit)
	props["CloseUnusedDirectMessages"] = strconv.FormatBool(*c.ServiceSettings.CloseUnusedDirectMessages)
	props["EnablePreviewFeatures"] = strconv.FormatBool(*c.ServiceSettings.EnablePreviewFeatures)
	props["EnableTutorial"] = strconv.FormatBool(*c.ServiceSettings.EnableTutorial)
	props["ExperimentalEnableDefaultChannelLeaveJoinMessages"] = strconv.FormatBool(*c.ServiceSettings.ExperimentalEnableDefaultChannelLeaveJoinMessages)
	props["ExperimentalGroupUnreadChannels"] = *c.ServiceSettings.ExperimentalGroupUnreadChannels
	props["ExperimentalEnableAutomaticReplies"] = strconv.FormatBool(*c.TeamSettings.ExperimentalEnableAutomaticReplies)
	props["ExperimentalTimezone"] = strconv.FormatBool(*c.DisplaySettings.ExperimentalTimezone)

	props["SendEmailNotifications"] = strconv.FormatBool(c.EmailSettings.SendEmailNotifications)
	props["SendPushNotifications"] = strconv.FormatBool(*c.EmailSettings.SendPushNotifications)
	props["EnableSignUpWithEmail"] = strconv.FormatBool(c.EmailSettings.EnableSignUpWithEmail)
	props["EnableSignInWithEmail"] = strconv.FormatBool(*c.EmailSettings.EnableSignInWithEmail)
	props["EnableSignInWithUsername"] = strconv.FormatBool(*c.EmailSettings.EnableSignInWithUsername)
	props["RequireEmailVerification"] = strconv.FormatBool(c.EmailSettings.RequireEmailVerification)
	props["EnableEmailBatching"] = strconv.FormatBool(*c.EmailSettings.EnableEmailBatching)
	props["EmailNotificationContentsType"] = *c.EmailSettings.EmailNotificationContentsType

	props["EmailLoginButtonColor"] = *c.EmailSettings.LoginButtonColor
	props["EmailLoginButtonBorderColor"] = *c.EmailSettings.LoginButtonBorderColor
	props["EmailLoginButtonTextColor"] = *c.EmailSettings.LoginButtonTextColor

	props["EnableSignUpWithGitLab"] = strconv.FormatBool(c.GitLabSettings.Enable)

	props["ShowEmailAddress"] = strconv.FormatBool(c.PrivacySettings.ShowEmailAddress)

	props["TermsOfServiceLink"] = *c.SupportSettings.TermsOfServiceLink
	props["PrivacyPolicyLink"] = *c.SupportSettings.PrivacyPolicyLink
	props["AboutLink"] = *c.SupportSettings.AboutLink
	props["HelpLink"] = *c.SupportSettings.HelpLink
	props["ReportAProblemLink"] = *c.SupportSettings.ReportAProblemLink
	props["SupportEmail"] = *c.SupportSettings.SupportEmail

	props["EnableFileAttachments"] = strconv.FormatBool(*c.FileSettings.EnableFileAttachments)
	props["EnablePublicLink"] = strconv.FormatBool(c.FileSettings.EnablePublicLink)

	props["WebsocketPort"] = fmt.Sprintf("%v", *c.ServiceSettings.WebsocketPort)
	props["WebsocketSecurePort"] = fmt.Sprintf("%v", *c.ServiceSettings.WebsocketSecurePort)

	props["DefaultClientLocale"] = *c.LocalizationSettings.DefaultClientLocale
	props["AvailableLocales"] = *c.LocalizationSettings.AvailableLocales
	props["SQLDriverName"] = *c.SqlSettings.DriverName

	props["EnableCustomEmoji"] = strconv.FormatBool(*c.ServiceSettings.EnableCustomEmoji)
	props["EnableEmojiPicker"] = strconv.FormatBool(*c.ServiceSettings.EnableEmojiPicker)
	props["RestrictCustomEmojiCreation"] = *c.ServiceSettings.RestrictCustomEmojiCreation
	props["MaxFileSize"] = strconv.FormatInt(*c.FileSettings.MaxFileSize, 10)
	props["AppDownloadLink"] = *c.NativeAppSettings.AppDownloadLink
	props["AndroidAppDownloadLink"] = *c.NativeAppSettings.AndroidAppDownloadLink
	props["IosAppDownloadLink"] = *c.NativeAppSettings.IosAppDownloadLink

	props["EnableWebrtc"] = strconv.FormatBool(*c.WebrtcSettings.Enable)

	props["MaxNotificationsPerChannel"] = strconv.FormatInt(*c.TeamSettings.MaxNotificationsPerChannel, 10)
	props["EnableConfirmNotificationsToChannel"] = strconv.FormatBool(*c.TeamSettings.EnableConfirmNotificationsToChannel)
	props["TimeBetweenUserTypingUpdatesMilliseconds"] = strconv.FormatInt(*c.ServiceSettings.TimeBetweenUserTypingUpdatesMilliseconds, 10)
	props["EnableUserTypingMessages"] = strconv.FormatBool(*c.ServiceSettings.EnableUserTypingMessages)
	props["EnableChannelViewedMessages"] = strconv.FormatBool(*c.ServiceSettings.EnableChannelViewedMessages)

	props["DiagnosticId"] = diagnosticId
	props["DiagnosticsEnabled"] = strconv.FormatBool(*c.LogSettings.EnableDiagnostics)

	props["PluginsEnabled"] = strconv.FormatBool(*c.PluginSettings.Enable)

	hasImageProxy := c.ServiceSettings.ImageProxyType != nil && *c.ServiceSettings.ImageProxyType != "" && c.ServiceSettings.ImageProxyURL != nil && *c.ServiceSettings.ImageProxyURL != ""
	props["HasImageProxy"] = strconv.FormatBool(hasImageProxy)

	// Set default values for all options that require a license.
	props["ExperimentalTownSquareIsReadOnly"] = "false"
	props["ExperimentalEnableAuthenticationTransfer"] = "true"
	props["EnableCustomBrand"] = "false"
	props["CustomBrandText"] = ""
	props["CustomDescriptionText"] = ""
	props["EnableLdap"] = "false"
	props["LdapLoginFieldName"] = ""
	props["LdapNicknameAttributeSet"] = "false"
	props["LdapFirstNameAttributeSet"] = "false"
	props["LdapLastNameAttributeSet"] = "false"
	props["LdapLoginButtonColor"] = ""
	props["LdapLoginButtonBorderColor"] = ""
	props["LdapLoginButtonTextColor"] = ""
	props["EnableMultifactorAuthentication"] = "false"
	props["EnforceMultifactorAuthentication"] = "false"
	props["EnableCompliance"] = "false"
	props["EnableMobileFileDownload"] = "true"
	props["EnableMobileFileUpload"] = "true"
	props["EnableSaml"] = "false"
	props["SamlLoginButtonText"] = ""
	props["SamlFirstNameAttributeSet"] = "false"
	props["SamlLastNameAttributeSet"] = "false"
	props["SamlNicknameAttributeSet"] = "false"
	props["SamlLoginButtonColor"] = ""
	props["SamlLoginButtonBorderColor"] = ""
	props["SamlLoginButtonTextColor"] = ""
	props["EnableCluster"] = "false"
	props["EnableMetrics"] = "false"
	props["EnableSignUpWithGoogle"] = "false"
	props["EnableSignUpWithOffice365"] = "false"
	props["PasswordMinimumLength"] = "0"
	props["PasswordRequireLowercase"] = "false"
	props["PasswordRequireUppercase"] = "false"
	props["PasswordRequireNumber"] = "false"
	props["PasswordRequireSymbol"] = "false"
	props["EnableBanner"] = "false"
	props["BannerText"] = ""
	props["BannerColor"] = ""
	props["BannerTextColor"] = ""
	props["AllowBannerDismissal"] = "false"
	props["EnableThemeSelection"] = "true"
	props["DefaultTheme"] = ""
	props["AllowCustomThemes"] = "true"
	props["AllowedThemes"] = ""
	props["DataRetentionEnableMessageDeletion"] = "false"
	props["DataRetentionMessageRetentionDays"] = "0"
	props["DataRetentionEnableFileDeletion"] = "false"
	props["DataRetentionFileRetentionDays"] = "0"

	if license != nil {
		props["ExperimentalTownSquareIsReadOnly"] = strconv.FormatBool(*c.TeamSettings.ExperimentalTownSquareIsReadOnly)
		props["ExperimentalEnableAuthenticationTransfer"] = strconv.FormatBool(*c.ServiceSettings.ExperimentalEnableAuthenticationTransfer)

		if *license.Features.CustomBrand {
			props["EnableCustomBrand"] = strconv.FormatBool(*c.TeamSettings.EnableCustomBrand)
			props["CustomBrandText"] = *c.TeamSettings.CustomBrandText
			props["CustomDescriptionText"] = *c.TeamSettings.CustomDescriptionText
		}

		if *license.Features.LDAP {
			props["EnableLdap"] = strconv.FormatBool(*c.LdapSettings.Enable)
			props["LdapLoginFieldName"] = *c.LdapSettings.LoginFieldName
			props["LdapNicknameAttributeSet"] = strconv.FormatBool(*c.LdapSettings.NicknameAttribute != "")
			props["LdapFirstNameAttributeSet"] = strconv.FormatBool(*c.LdapSettings.FirstNameAttribute != "")
			props["LdapLastNameAttributeSet"] = strconv.FormatBool(*c.LdapSettings.LastNameAttribute != "")
			props["LdapLoginButtonColor"] = *c.LdapSettings.LoginButtonColor
			props["LdapLoginButtonBorderColor"] = *c.LdapSettings.LoginButtonBorderColor
			props["LdapLoginButtonTextColor"] = *c.LdapSettings.LoginButtonTextColor
		}

		if *license.Features.MFA {
			props["EnableMultifactorAuthentication"] = strconv.FormatBool(*c.ServiceSettings.EnableMultifactorAuthentication)
			props["EnforceMultifactorAuthentication"] = strconv.FormatBool(*c.ServiceSettings.EnforceMultifactorAuthentication)
		}

		if *license.Features.Compliance {
			props["EnableCompliance"] = strconv.FormatBool(*c.ComplianceSettings.Enable)
			props["EnableMobileFileDownload"] = strconv.FormatBool(*c.FileSettings.EnableMobileDownload)
			props["EnableMobileFileUpload"] = strconv.FormatBool(*c.FileSettings.EnableMobileUpload)
		}

		if *license.Features.SAML {
			props["EnableSaml"] = strconv.FormatBool(*c.SamlSettings.Enable)
			props["SamlLoginButtonText"] = *c.SamlSettings.LoginButtonText
			props["SamlFirstNameAttributeSet"] = strconv.FormatBool(*c.SamlSettings.FirstNameAttribute != "")
			props["SamlLastNameAttributeSet"] = strconv.FormatBool(*c.SamlSettings.LastNameAttribute != "")
			props["SamlNicknameAttributeSet"] = strconv.FormatBool(*c.SamlSettings.NicknameAttribute != "")
			props["SamlLoginButtonColor"] = *c.SamlSettings.LoginButtonColor
			props["SamlLoginButtonBorderColor"] = *c.SamlSettings.LoginButtonBorderColor
			props["SamlLoginButtonTextColor"] = *c.SamlSettings.LoginButtonTextColor
		}

		if *license.Features.Cluster {
			props["EnableCluster"] = strconv.FormatBool(*c.ClusterSettings.Enable)
		}

		if *license.Features.Cluster {
			props["EnableMetrics"] = strconv.FormatBool(*c.MetricsSettings.Enable)
		}

		if *license.Features.GoogleOAuth {
			props["EnableSignUpWithGoogle"] = strconv.FormatBool(c.GoogleSettings.Enable)
		}

		if *license.Features.Office365OAuth {
			props["EnableSignUpWithOffice365"] = strconv.FormatBool(c.Office365Settings.Enable)
		}

		if *license.Features.PasswordRequirements {
			props["PasswordMinimumLength"] = fmt.Sprintf("%v", *c.PasswordSettings.MinimumLength)
			props["PasswordRequireLowercase"] = strconv.FormatBool(*c.PasswordSettings.Lowercase)
			props["PasswordRequireUppercase"] = strconv.FormatBool(*c.PasswordSettings.Uppercase)
			props["PasswordRequireNumber"] = strconv.FormatBool(*c.PasswordSettings.Number)
			props["PasswordRequireSymbol"] = strconv.FormatBool(*c.PasswordSettings.Symbol)
		}

		if *license.Features.Announcement {
			props["EnableBanner"] = strconv.FormatBool(*c.AnnouncementSettings.EnableBanner)
			props["BannerText"] = *c.AnnouncementSettings.BannerText
			props["BannerColor"] = *c.AnnouncementSettings.BannerColor
			props["BannerTextColor"] = *c.AnnouncementSettings.BannerTextColor
			props["AllowBannerDismissal"] = strconv.FormatBool(*c.AnnouncementSettings.AllowBannerDismissal)
		}

		if *license.Features.ThemeManagement {
			props["EnableThemeSelection"] = strconv.FormatBool(*c.ThemeSettings.EnableThemeSelection)
			props["DefaultTheme"] = *c.ThemeSettings.DefaultTheme
			props["AllowCustomThemes"] = strconv.FormatBool(*c.ThemeSettings.AllowCustomThemes)
			props["AllowedThemes"] = strings.Join(c.ThemeSettings.AllowedThemes, ",")
		}

		if *license.Features.DataRetention {
			props["DataRetentionEnableMessageDeletion"] = strconv.FormatBool(*c.DataRetentionSettings.EnableMessageDeletion)
			props["DataRetentionMessageRetentionDays"] = strconv.FormatInt(int64(*c.DataRetentionSettings.MessageRetentionDays), 10)
			props["DataRetentionEnableFileDeletion"] = strconv.FormatBool(*c.DataRetentionSettings.EnableFileDeletion)
			props["DataRetentionFileRetentionDays"] = strconv.FormatInt(int64(*c.DataRetentionSettings.FileRetentionDays), 10)
		}
	}

	return props
}

func ValidateLdapFilter(cfg *model.Config, ldap einterfaces.LdapInterface) *model.AppError {
	if *cfg.LdapSettings.Enable && ldap != nil && *cfg.LdapSettings.UserFilter != "" {
		if err := ldap.ValidateFilter(*cfg.LdapSettings.UserFilter); err != nil {
			return err
		}
	}
	return nil
}

func ValidateLocales(cfg *model.Config) *model.AppError {
	var err *model.AppError
	locales := GetSupportedLocales()
	if _, ok := locales[*cfg.LocalizationSettings.DefaultServerLocale]; !ok {
		*cfg.LocalizationSettings.DefaultServerLocale = model.DEFAULT_LOCALE
		err = model.NewAppError("ValidateLocales", "utils.config.supported_server_locale.app_error", nil, "", http.StatusBadRequest)
	}

	if _, ok := locales[*cfg.LocalizationSettings.DefaultClientLocale]; !ok {
		*cfg.LocalizationSettings.DefaultClientLocale = model.DEFAULT_LOCALE
		err = model.NewAppError("ValidateLocales", "utils.config.supported_client_locale.app_error", nil, "", http.StatusBadRequest)
	}

	if len(*cfg.LocalizationSettings.AvailableLocales) > 0 {
		isDefaultClientLocaleInAvailableLocales := false
		for _, word := range strings.Split(*cfg.LocalizationSettings.AvailableLocales, ",") {
			if _, ok := locales[word]; !ok {
				*cfg.LocalizationSettings.AvailableLocales = ""
				isDefaultClientLocaleInAvailableLocales = true
				err = model.NewAppError("ValidateLocales", "utils.config.supported_available_locales.app_error", nil, "", http.StatusBadRequest)
				break
			}

			if word == *cfg.LocalizationSettings.DefaultClientLocale {
				isDefaultClientLocaleInAvailableLocales = true
			}
		}

		availableLocales := *cfg.LocalizationSettings.AvailableLocales

		if !isDefaultClientLocaleInAvailableLocales {
			availableLocales += "," + *cfg.LocalizationSettings.DefaultClientLocale
			err = model.NewAppError("ValidateLocales", "utils.config.add_client_locale.app_error", nil, "", http.StatusBadRequest)
		}

		*cfg.LocalizationSettings.AvailableLocales = strings.Join(RemoveDuplicatesFromStringArray(strings.Split(availableLocales, ",")), ",")
	}

	return err
}

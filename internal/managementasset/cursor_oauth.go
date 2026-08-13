package managementasset

import (
	"bytes"
	"fmt"
	"regexp"
)

var cursorOAuthLocales = []struct {
	marker      []byte
	replacement []byte
}{
	{
		marker: []byte("xai_oauth_polling_error:`检查认证状态失败:`,plugin_oauth_title:"),
		replacement: []byte(
			"xai_oauth_polling_error:`检查认证状态失败:`," +
				"cursor_oauth_title:`Cursor OAuth`," +
				"cursor_oauth_button:`开始 Cursor 登录`," +
				"cursor_oauth_hint:`通过 Cursor 深度控制授权登录，自动获取并保存认证文件。`," +
				"cursor_oauth_url_label:`授权链接:`," +
				"cursor_open_link:`打开链接`," +
				"cursor_copy_link:`复制链接`," +
				"cursor_oauth_status_waiting:`等待认证中...`," +
				"cursor_oauth_status_success:`认证成功！`," +
				"cursor_oauth_status_error:`认证失败:`," +
				"cursor_oauth_start_error:`启动 Cursor OAuth 失败:`," +
				"cursor_oauth_polling_error:`检查认证状态失败:`," +
				"plugin_oauth_title:",
		),
	},
	{
		marker: []byte("xai_oauth_polling_error:`檢查驗證狀態失敗:`,plugin_oauth_title:"),
		replacement: []byte(
			"xai_oauth_polling_error:`檢查驗證狀態失敗:`," +
				"cursor_oauth_title:`Cursor OAuth`," +
				"cursor_oauth_button:`開始 Cursor 登入`," +
				"cursor_oauth_hint:`透過 Cursor 深度控制授權登入，自動取得並儲存認證檔案。`," +
				"cursor_oauth_url_label:`授權連結:`," +
				"cursor_open_link:`開啟連結`," +
				"cursor_copy_link:`複製連結`," +
				"cursor_oauth_status_waiting:`等待認證中...`," +
				"cursor_oauth_status_success:`認證成功！`," +
				"cursor_oauth_status_error:`認證失敗:`," +
				"cursor_oauth_start_error:`啟動 Cursor OAuth 失敗:`," +
				"cursor_oauth_polling_error:`檢查驗證狀態失敗:`," +
				"plugin_oauth_title:",
		),
	},
	{
		marker: []byte("xai_oauth_polling_error:`Failed to check authentication status:`,plugin_oauth_title:"),
		replacement: []byte(
			"xai_oauth_polling_error:`Failed to check authentication status:`," +
				"cursor_oauth_title:`Cursor OAuth`," +
				"cursor_oauth_button:`Start Cursor Login`," +
				"cursor_oauth_hint:`Login to Cursor through deep-control authorization and automatically save the credentials.`," +
				"cursor_oauth_url_label:`Authorization URL:`," +
				"cursor_open_link:`Open Link`," +
				"cursor_copy_link:`Copy Link`," +
				"cursor_oauth_status_waiting:`Waiting for authentication...`," +
				"cursor_oauth_status_success:`Authentication successful!`," +
				"cursor_oauth_status_error:`Authentication failed:`," +
				"cursor_oauth_start_error:`Failed to start Cursor OAuth:`," +
				"cursor_oauth_polling_error:`Failed to check authentication status:`," +
				"plugin_oauth_title:",
		),
	},
}

var remainingCursorOAuthLocale = regexp.MustCompile(
	"xai_oauth_polling_error:`([^`]*)`,plugin_oauth_title:",
)

var fallbackCursorOAuthLocale = []byte(
	"cursor_oauth_title:`Cursor OAuth`," +
		"cursor_oauth_button:`Start Cursor Login`," +
		"cursor_oauth_hint:`Login to Cursor through deep-control authorization and automatically save the credentials.`," +
		"cursor_oauth_url_label:`Authorization URL:`," +
		"cursor_open_link:`Open Link`," +
		"cursor_copy_link:`Copy Link`," +
		"cursor_oauth_status_waiting:`Waiting for authentication...`," +
		"cursor_oauth_status_success:`Authentication successful!`," +
		"cursor_oauth_status_error:`Authentication failed:`," +
		"cursor_oauth_start_error:`Failed to start Cursor OAuth:`," +
		"cursor_oauth_polling_error:`Failed to check authentication status:`,",
)

// AddCursorOAuthToManagementHTML adapts the upstream management bundle to the
// Cursor OAuth endpoint exposed by this server. The upstream asset is kept
// byte-for-byte on disk; adaptation happens when the panel is served so an
// automatic asset refresh cannot remove the provider again.
func AddCursorOAuthToManagementHTML(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("empty management html")
	}
	if bytes.Contains(data, []byte("id:`cursor`,titleKey:`auth_login.cursor_oauth_title`")) {
		return data, nil
	}

	providerMarker := []byte("{kind:`builtin`,id:`xai`,titleKey:`auth_login.xai_oauth_title`,icon:")
	providerStart := bytes.Index(data, providerMarker)
	if providerStart < 0 {
		return nil, fmt.Errorf("management html Cursor OAuth provider anchor not found")
	}
	providerEndRelative := bytes.Index(data[providerStart:], []byte("}}]"))
	if providerEndRelative < 0 {
		return nil, fmt.Errorf("management html Cursor OAuth provider boundary not found")
	}
	providerEnd := providerStart + providerEndRelative + 2
	provider := append([]byte(nil), data[providerStart:providerEnd]...)
	provider = bytes.Replace(provider, []byte("id:`xai`"), []byte("id:`cursor`"), 1)
	provider = bytes.Replace(provider, []byte("auth_login.xai_oauth_title"), []byte("auth_login.cursor_oauth_title"), 1)
	data = insertBytes(data, providerEnd, append([]byte(","), provider...))

	patchedLocales := 0
	for _, locale := range cursorOAuthLocales {
		if bytes.Contains(data, locale.marker) {
			data = bytes.Replace(data, locale.marker, locale.replacement, 1)
			patchedLocales++
		}
	}
	data = remainingCursorOAuthLocale.ReplaceAllFunc(data, func(match []byte) []byte {
		patchedLocales++
		prefixEnd := bytes.Index(match, []byte("plugin_oauth_title:"))
		if prefixEnd < 0 {
			return match
		}
		result := make([]byte, 0, len(match)+len(fallbackCursorOAuthLocale))
		result = append(result, match[:prefixEnd]...)
		result = append(result, fallbackCursorOAuthLocale...)
		result = append(result, match[prefixEnd:]...)
		return result
	})
	if patchedLocales == 0 {
		return nil, fmt.Errorf("management html Cursor OAuth locale anchor not found")
	}
	return data, nil
}

func insertBytes(data []byte, index int, addition []byte) []byte {
	result := make([]byte, 0, len(data)+len(addition))
	result = append(result, data[:index]...)
	result = append(result, addition...)
	result = append(result, data[index:]...)
	return result
}

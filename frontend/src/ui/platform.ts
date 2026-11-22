// The OS the window runs on, for wording that names it. WKWebView on macOS
// reports "Macintosh" in its user agent; WebView2 on Windows does not.
export const isMac = /Mac/.test(navigator.userAgent);

// Where keys added in the app are stored.
export const vaultName = isMac ? 'the macOS Keychain' : 'Windows Credential Manager';

// The OS name in "Match …" for the system theme.
export const osName = isMac ? 'macOS' : 'Windows';

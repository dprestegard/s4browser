import {useCallback, useEffect, useState} from 'react';
import {WindowSetDarkTheme, WindowSetLightTheme, WindowSetSystemDefaultTheme} from '../wailsjs/runtime/runtime';

// View preferences. They are per machine conveniences, so they live in the
// webview's localStorage; nothing here is needed for correctness, and every
// access tolerates storage being unavailable.

export type ThemeSetting = 'dark' | 'light' | 'system';
export type Density = 'compact' | 'comfortable';

function read<T>(key: string, fallback: T): T {
    try {
        const raw = localStorage.getItem('s4:' + key);
        return raw === null ? fallback : (JSON.parse(raw) as T);
    } catch {
        return fallback;
    }
}

function write(key: string, value: unknown) {
    try {
        localStorage.setItem('s4:' + key, JSON.stringify(value));
    } catch {
        // Storage refused: the setting lasts for this session only.
    }
}

export function useSetting<T>(key: string, fallback: T): [T, (v: T) => void] {
    const [value, setValue] = useState<T>(() => read(key, fallback));
    const set = useCallback((v: T) => {
        setValue(v);
        write(key, v);
    }, [key]);
    return [value, set];
}

const systemDark = () => window.matchMedia?.('(prefers-color-scheme: dark)').matches ?? true;

// Applies the theme to the page and to the native title bar.
export function useAppliedTheme(theme: ThemeSetting, density: Density) {
    const [dark, setDark] = useState(systemDark);
    useEffect(() => {
        const mq = window.matchMedia?.('(prefers-color-scheme: dark)');
        if (!mq) return;
        const on = () => setDark(mq.matches);
        mq.addEventListener('change', on);
        return () => mq.removeEventListener('change', on);
    }, []);

    const effective = theme === 'system' ? (dark ? 'dark' : 'light') : theme;
    useEffect(() => {
        document.documentElement.dataset.theme = effective;
        try {
            if (theme === 'system') WindowSetSystemDefaultTheme();
            else if (theme === 'dark') WindowSetDarkTheme();
            else WindowSetLightTheme();
        } catch {
            // Not running inside Wails (plain browser during development).
        }
    }, [theme, effective]);

    useEffect(() => {
        document.documentElement.dataset.density = density;
    }, [density]);
}

// Reads a setting synchronously before React starts, so the first paint already
// has the right theme.
export function applyInitialTheme() {
    const theme = read<ThemeSetting>('theme', 'dark');
    document.documentElement.dataset.theme = theme === 'system' ? (systemDark() ? 'dark' : 'light') : theme;
    document.documentElement.dataset.density = read<Density>('density', 'compact');
}

// The open location survives a reload of the window (the WebView's Refresh) but
// not a restart, so it lives in sessionStorage. It is a convenience: when
// storage is unavailable the window opens with no bucket, as before.
export type SavedLocation = {profileId: string; bucket: string; prefix: string; region?: string};

export function readSavedLocation(): SavedLocation | null {
    try {
        const raw = sessionStorage.getItem('s4:location');
        const v = raw ? JSON.parse(raw) as SavedLocation : null;
        return v && typeof v.profileId === 'string' && typeof v.bucket === 'string' && typeof v.prefix === 'string' ? v : null;
    } catch {
        return null;
    }
}

export function saveLocation(v: SavedLocation | null) {
    try {
        if (v) sessionStorage.setItem('s4:location', JSON.stringify(v));
        else sessionStorage.removeItem('s4:location');
    } catch {
        // Storage refused: a reload opens with no bucket.
    }
}

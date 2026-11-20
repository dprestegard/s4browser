// Display formats shared by the shell. Sizes are binary units with one decimal,
// as in the approved design ("312.4 GiB"); dates are local time, "2026-11-12 14:03".

const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB'];

export function formatSize(n: number): string {
    if (!Number.isFinite(n) || n < 1024) return `${n} B`;
    let v = n;
    let i = 0;
    while (v >= 1024 && i < units.length - 1) {
        v /= 1024;
        i++;
    }
    // 1023.96 GiB would print as "1024.0 GiB"; step up a unit instead.
    if (v >= 1023.95 && i < units.length - 1) {
        v /= 1024;
        i++;
    }
    // One decimal, except small TiB and PiB figures, where 0.1 is a lot of data.
    const decimals = i >= 4 && v < 10 ? 2 : 1;
    return `${v.toFixed(decimals)} ${units[i]}`;
}

export function formatBytes(n: number): string {
    return `${n.toLocaleString('en-US')} bytes`;
}

const pad = (n: number) => String(n).padStart(2, '0');

export function formatDate(value: unknown, seconds = false): string {
    if (!value) return '';
    const d = new Date(value as string);
    if (Number.isNaN(d.getTime()) || d.getUTCFullYear() < 1971) return '';
    const s = `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
    return seconds ? `${s}:${pad(d.getSeconds())}` : s;
}

// "5 h 12 m", "43 min", "under a minute". Negative means already past.
export function formatRemaining(ms: number): string {
    if (ms <= 0) return 'expired';
    const min = Math.floor(ms / 60000);
    if (min < 1) return 'under a minute';
    if (min < 60) return `${min} min`;
    const h = Math.floor(min / 60);
    if (h < 48) return `${h} h ${min % 60} m`;
    return `${Math.floor(h / 24)} days`;
}

const classNames: Record<string, string> = {
    STANDARD: 'Standard',
    INTELLIGENT_TIERING: 'Intelligent-Tiering',
    STANDARD_IA: 'Standard-IA',
    ONEZONE_IA: 'One Zone-IA',
    GLACIER_IR: 'Glacier Instant Retrieval',
    GLACIER: 'Glacier Flexible Retrieval',
    DEEP_ARCHIVE: 'Glacier Deep Archive',
    REDUCED_REDUNDANCY: 'Reduced Redundancy',
    EXPRESS_ONEZONE: 'Express One Zone',
    OUTPOSTS: 'Outposts',
    SNOW: 'Snow',
};

export function formatStorageClass(c?: string): string {
    if (!c) return '';
    return classNames[c] ?? c;
}

export function plural(n: number, one: string, many = one + 's'): string {
    return `${n.toLocaleString('en-US')} ${n === 1 ? one : many}`;
}

// "412.3 MiB/s". Zero or unknown is a dash.
export function formatRate(bytesPerSec: number): string {
    if (!Number.isFinite(bytesPerSec) || bytesPerSec <= 0) return '–';
    return `${formatSize(Math.round(bytesPerSec))}/s`;
}

// Time left for a transfer: "45 s", "12 min", "3 h 05 m". Negative is unknown.
export function formatEta(seconds: number): string {
    if (!Number.isFinite(seconds) || seconds < 0) return '–';
    if (seconds < 60) return `${Math.max(1, Math.round(seconds))} s`;
    const min = Math.round(seconds / 60);
    if (min < 60) return `${min} min`;
    const h = Math.floor(min / 60);
    if (h < 48) return `${h} h ${String(min % 60).padStart(2, '0')} m`;
    return `${Math.floor(h / 24)} days`;
}

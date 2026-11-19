// Shapes of what the Go bindings return. They mirror the JSON of the Go types
// (wailsjs/go/models.ts holds the generated classes).

export type Profile = {
    id: string;
    name: string;
    source: string; // "app" or "aws"
    kind: string;
    region?: string;
    keyHint?: string;
    supported: boolean;
    problem?: string;
};

export type ProfileState = {profiles: Profile[]; activeId: string; warnings?: string[]; error?: string};

export type CredentialStatus =
    | {state: 'none'}
    | {state: 'checking'}
    | {state: 'ok'; expires?: number; keyHint?: string}
    | {state: 'failed'; message: string};

export type Bucket = {name: string; region?: string; listed: boolean; added: boolean};
export type BucketList = {buckets: Bucket[]; listError?: string; listDenied?: boolean};

export type Snapshot = {
    id: string;
    profile: string;
    bucket: string;
    prefix: string;
    region?: string;
    state: 'loading' | 'done' | 'error' | 'cancelled';
    count: number;
    folders: number;
    objects: number;
    pages: number;
    truncated?: boolean;
    error?: string;
    version: number;
    elapsedMs: number;
};

export type Row = {key: string; name: string; folder: boolean; size: number; modified?: string; storageClass?: string};
export type ListingWindow = {listing: Snapshot; offset: number; rows: Row[]};
export type SortField = 'name' | 'size' | 'modified' | 'class';
export type Sort = {field: SortField; desc: boolean};

export type Location = {bucket: string; prefix: string; region?: string};

export type DroppedItem = {path: string; kind: string; absolute: boolean; error?: string};
export type Drop = {at: string; x: number; y: number; items: DroppedItem[]; logError?: string};

export const kindLabels: Record<string, string> = {
    'vault-keys': 'Keys in vault',
    static: 'Static keys',
    session: 'Session keys',
    'credential-process': 'credential_process',
    other: 'Unsupported',
    none: 'No credentials',
};

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
// What the UI knows about the selection. The selection itself is index ranges
// in Browser; Go totals them, so unrendered rows never travel to the page.
export type SelectionInfo = {count: number; folders: number; objects: number; bytes: number; single: Row | null};
export const noSelection: SelectionInfo = {count: 0, folders: 0, objects: 0, bytes: 0, single: null};
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
    login: 'aws login',
    role: 'Assumed role',
    other: 'Unsupported',
    none: 'No credentials',
};

// Transfers (internal/queue). JobView arrives in "s4:queue" events and from
// QueueJobs; merge by id.
export type JobState =
    | 'queued' | 'running' | 'pausing' | 'paused' | 'credential-wait'
    | 'cancelling' | 'failing' | 'cancelled' | 'failed' | 'completed';

export type JobView = {
    id: string;
    direction: 'upload' | 'download';
    profile: string;
    bucket: string;
    key: string;
    localPath: string;
    name: string;
    state: JobState;
    phase: 'preparing' | 'transferring' | 'finalizing';
    totalBytes: number;
    doneBytes: number;
    bytesPerSec: number;
    etaSeconds: number;
    partsDone: number;
    partsTotal: number;
    retries: number;
    reason?: string;
    errorCode?: string;
    detail?: string;
    warning?: string;
    finished: boolean;
    canPause: boolean;
    canResume: boolean;
    canCancel: boolean;
    canRetry: boolean;
    canRemove: boolean;
};

export type QueueEvent = {seq: number; at: number; jobs: JobView[] | null; removed?: string[]; resync?: boolean};

export type CleanupStatus = {running: boolean; finished: boolean; uploadsAborted: number; tempsRemoved: number; pending: number; error?: string};
export type QueueStatus = {
    available: boolean;
    error?: string;
    uploadsDisabled: boolean;
    uploadsDisabledReason?: string;
    warnings: string[] | null;
    cleanup: CleanupStatus;
};

export type TransferSettings = {
    parallelFiles: number;
    partsPerFile: number;
    totalParts: number;
    partSize: number;
    uploadBytesPerSec: number;
    downloadBytesPerSec: number;
};
export type SettingsLimits = {
    minParallelFiles: number; maxParallelFiles: number;
    minPartsPerFile: number; maxPartsPerFile: number;
    minTotalParts: number; maxTotalParts: number;
    minPartSize: number; maxPartSize: number;
};
export type SettingsState = {current: TransferSettings; defaults: TransferSettings; limits: SettingsLimits};

export type Refusal = {path: string; code: string; reason: string};
export type SubmitReport = {
    requested: number;
    queued: number;
    refused: Refusal[] | null;
    refusedTotal: number;
    emptyFolders: number;
    placeholders: number;
    examined: number;
};

// What the listing hands over when the user downloads its selection: index
// ranges of one sort order of one listing version. Go resolves them to keys.
export type SelectionRequest = {
    listingId: string;
    field: SortField;
    desc: boolean;
    version: number;
    ranges: [number, number][];
    count: number;
    folders: number;
    objects: number;
    bytes: number;
};

// Object commands (stage 3.1).
export type DeletePreview = {
    id: string;
    bucket: string;
    prefix: string;
    objects: number;
    bytes: number;
    folders: number;
    selected: number;
    placeholders: number;
    sample: string[] | null;
    versioning: 'off' | 'enabled' | 'suspended' | 'unknown';
    versioningError?: string;
    transfers: number;
};
export type DeleteFailure = {key: string; code?: string; message: string};
export type DeleteResult = {
    requested: number;
    deleted: number;
    markers: number;
    failed: DeleteFailure[] | null;
    failedTotal: number;
    notTried: number;
    stopped?: string;
};
export type ObjectProgress = {phase: 'counting' | 'deleting'; done: number; total: number};
export type Presigned = {
    url: string;
    expires: string;
    requestedSeconds: number;
    effectiveSeconds: number;
    limitedBy?: 'maximum' | 'credentials';
    credentialsExpire?: string;
};
export type ObjectMetadata = {
    key: string;
    size: number;
    etag: string;
    parts?: number;
    lastModified?: string;
    storageClass: string;
    contentType?: string;
    cacheControl?: string;
    contentDisposition?: string;
    contentEncoding?: string;
    contentLanguage?: string;
    expires?: string;
    versionId?: string;
    encryption?: string;
    kmsKeyId?: string;
    checksum?: string;
    tagCount: number;
    restore?: string;
    objectLock?: string;
    metadata: Record<string, string>;
};
export type Tag = {key: string; value: string};

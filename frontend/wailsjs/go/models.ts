export namespace browse {
	
	export class Bucket {
	    name: string;
	    region?: string;
	    listed: boolean;
	    added: boolean;
	    // Go type: time
	    created?: any;
	
	    static createFrom(source: any = {}) {
	        return new Bucket(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.region = source["region"];
	        this.listed = source["listed"];
	        this.added = source["added"];
	        this.created = this.convertValues(source["created"], null);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class AddResult {
	    bucket: Bucket;
	    note?: string;
	
	    static createFrom(source: any = {}) {
	        return new AddResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.bucket = this.convertValues(source["bucket"], Bucket);
	        this.note = source["note"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	
	export class BucketList {
	    buckets: Bucket[];
	    listError?: string;
	    listDenied?: boolean;
	
	    static createFrom(source: any = {}) {
	        return new BucketList(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.buckets = this.convertValues(source["buckets"], Bucket);
	        this.listError = source["listError"];
	        this.listDenied = source["listDenied"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class Row {
	    key: string;
	    name: string;
	    folder: boolean;
	    size: number;
	    // Go type: time
	    modified?: any;
	    storageClass?: string;
	
	    static createFrom(source: any = {}) {
	        return new Row(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.key = source["key"];
	        this.name = source["name"];
	        this.folder = source["folder"];
	        this.size = source["size"];
	        this.modified = this.convertValues(source["modified"], null);
	        this.storageClass = source["storageClass"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class Snapshot {
	    id: string;
	    profile: string;
	    bucket: string;
	    prefix: string;
	    region?: string;
	    state: string;
	    count: number;
	    folders: number;
	    objects: number;
	    pages: number;
	    truncated?: boolean;
	    error?: string;
	    version: number;
	    elapsedMs: number;
	
	    static createFrom(source: any = {}) {
	        return new Snapshot(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.profile = source["profile"];
	        this.bucket = source["bucket"];
	        this.prefix = source["prefix"];
	        this.region = source["region"];
	        this.state = source["state"];
	        this.count = source["count"];
	        this.folders = source["folders"];
	        this.objects = source["objects"];
	        this.pages = source["pages"];
	        this.truncated = source["truncated"];
	        this.error = source["error"];
	        this.version = source["version"];
	        this.elapsedMs = source["elapsedMs"];
	    }
	}
	export class SortSpec {
	    field: string;
	    desc: boolean;
	
	    static createFrom(source: any = {}) {
	        return new SortSpec(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.field = source["field"];
	        this.desc = source["desc"];
	    }
	}
	export class Summary {
	    count: number;
	    folders: number;
	    objects: number;
	    bytes: number;
	
	    static createFrom(source: any = {}) {
	        return new Summary(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.count = source["count"];
	        this.folders = source["folders"];
	        this.objects = source["objects"];
	        this.bytes = source["bytes"];
	    }
	}
	export class Window {
	    listing: Snapshot;
	    sort: SortSpec;
	    offset: number;
	    rows: Row[];
	
	    static createFrom(source: any = {}) {
	        return new Window(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.listing = this.convertValues(source["listing"], Snapshot);
	        this.sort = this.convertValues(source["sort"], SortSpec);
	        this.offset = source["offset"];
	        this.rows = this.convertValues(source["rows"], Row);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

}

export namespace creds {
	
	export class Profile {
	    id: string;
	    name: string;
	    source: string;
	    kind: string;
	    region?: string;
	    keyHint?: string;
	    supported: boolean;
	    problem?: string;
	
	    static createFrom(source: any = {}) {
	        return new Profile(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.source = source["source"];
	        this.kind = source["kind"];
	        this.region = source["region"];
	        this.keyHint = source["keyHint"];
	        this.supported = source["supported"];
	        this.problem = source["problem"];
	    }
	}

}

export namespace main {
	
	export class DeletePreview {
	    id: string;
	    bucket: string;
	    prefix: string;
	    objects: number;
	    bytes: number;
	    folders: number;
	    selected: number;
	    placeholders: number;
	    sample: string[];
	    versioning: string;
	    versioningError?: string;
	    transfers: number;
	
	    static createFrom(source: any = {}) {
	        return new DeletePreview(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.bucket = source["bucket"];
	        this.prefix = source["prefix"];
	        this.objects = source["objects"];
	        this.bytes = source["bytes"];
	        this.folders = source["folders"];
	        this.selected = source["selected"];
	        this.placeholders = source["placeholders"];
	        this.sample = source["sample"];
	        this.versioning = source["versioning"];
	        this.versioningError = source["versioningError"];
	        this.transfers = source["transfers"];
	    }
	}
	export class DownloadSelection {
	    profileId: string;
	    bucket: string;
	    region: string;
	    listingId: string;
	    field: string;
	    desc: boolean;
	    version: number;
	    ranges: number[][];
	    destDir: string;
	
	    static createFrom(source: any = {}) {
	        return new DownloadSelection(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.profileId = source["profileId"];
	        this.bucket = source["bucket"];
	        this.region = source["region"];
	        this.listingId = source["listingId"];
	        this.field = source["field"];
	        this.desc = source["desc"];
	        this.version = source["version"];
	        this.ranges = source["ranges"];
	        this.destDir = source["destDir"];
	    }
	}
	export class ObjectSelection {
	    profileId: string;
	    bucket: string;
	    listingId: string;
	    field: string;
	    desc: boolean;
	    version: number;
	    ranges: number[][];
	
	    static createFrom(source: any = {}) {
	        return new ObjectSelection(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.profileId = source["profileId"];
	        this.bucket = source["bucket"];
	        this.listingId = source["listingId"];
	        this.field = source["field"];
	        this.desc = source["desc"];
	        this.version = source["version"];
	        this.ranges = source["ranges"];
	    }
	}
	export class ProfileCheck {
	    ok: boolean;
	    message?: string;
	    keyHint?: string;
	    // Go type: time
	    expires?: any;
	
	    static createFrom(source: any = {}) {
	        return new ProfileCheck(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.ok = source["ok"];
	        this.message = source["message"];
	        this.keyHint = source["keyHint"];
	        this.expires = this.convertValues(source["expires"], null);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class ProfileState {
	    profiles: creds.Profile[];
	    activeId: string;
	    warnings?: string[];
	    error?: string;
	
	    static createFrom(source: any = {}) {
	        return new ProfileState(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.profiles = this.convertValues(source["profiles"], creds.Profile);
	        this.activeId = source["activeId"];
	        this.warnings = source["warnings"];
	        this.error = source["error"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class StartInfo {
	    logPath: string;
	    logError?: string;
	
	    static createFrom(source: any = {}) {
	        return new StartInfo(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.logPath = source["logPath"];
	        this.logError = source["logError"];
	    }
	}

}

export namespace objects {
	
	export class Failure {
	    key: string;
	    code?: string;
	    message: string;
	
	    static createFrom(source: any = {}) {
	        return new Failure(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.key = source["key"];
	        this.code = source["code"];
	        this.message = source["message"];
	    }
	}
	export class Metadata {
	    key: string;
	    size: number;
	    etag: string;
	    parts?: number;
	    // Go type: time
	    lastModified?: any;
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
	
	    static createFrom(source: any = {}) {
	        return new Metadata(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.key = source["key"];
	        this.size = source["size"];
	        this.etag = source["etag"];
	        this.parts = source["parts"];
	        this.lastModified = this.convertValues(source["lastModified"], null);
	        this.storageClass = source["storageClass"];
	        this.contentType = source["contentType"];
	        this.cacheControl = source["cacheControl"];
	        this.contentDisposition = source["contentDisposition"];
	        this.contentEncoding = source["contentEncoding"];
	        this.contentLanguage = source["contentLanguage"];
	        this.expires = source["expires"];
	        this.versionId = source["versionId"];
	        this.encryption = source["encryption"];
	        this.kmsKeyId = source["kmsKeyId"];
	        this.checksum = source["checksum"];
	        this.tagCount = source["tagCount"];
	        this.restore = source["restore"];
	        this.objectLock = source["objectLock"];
	        this.metadata = source["metadata"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class Presigned {
	    url: string;
	    // Go type: time
	    expires: any;
	    requestedSeconds: number;
	    effectiveSeconds: number;
	    limitedBy?: string;
	    // Go type: time
	    credentialsExpire?: any;
	
	    static createFrom(source: any = {}) {
	        return new Presigned(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.url = source["url"];
	        this.expires = this.convertValues(source["expires"], null);
	        this.requestedSeconds = source["requestedSeconds"];
	        this.effectiveSeconds = source["effectiveSeconds"];
	        this.limitedBy = source["limitedBy"];
	        this.credentialsExpire = this.convertValues(source["credentialsExpire"], null);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class Result {
	    requested: number;
	    deleted: number;
	    markers: number;
	    failed: Failure[];
	    failedTotal: number;
	    notTried: number;
	    stopped?: string;
	
	    static createFrom(source: any = {}) {
	        return new Result(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.requested = source["requested"];
	        this.deleted = source["deleted"];
	        this.markers = source["markers"];
	        this.failed = this.convertValues(source["failed"], Failure);
	        this.failedTotal = source["failedTotal"];
	        this.notTried = source["notTried"];
	        this.stopped = source["stopped"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class Tag {
	    key: string;
	    value: string;
	
	    static createFrom(source: any = {}) {
	        return new Tag(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.key = source["key"];
	        this.value = source["value"];
	    }
	}

}

export namespace queue {
	
	export class CleanupStatus {
	    running: boolean;
	    finished: boolean;
	    uploadsAborted: number;
	    tempsRemoved: number;
	    pending: number;
	    error?: string;
	
	    static createFrom(source: any = {}) {
	        return new CleanupStatus(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.running = source["running"];
	        this.finished = source["finished"];
	        this.uploadsAborted = source["uploadsAborted"];
	        this.tempsRemoved = source["tempsRemoved"];
	        this.pending = source["pending"];
	        this.error = source["error"];
	    }
	}
	export class DownloadRequest {
	    profileId: string;
	    bucket: string;
	    region: string;
	    keys: string[];
	    destDir: string;
	
	    static createFrom(source: any = {}) {
	        return new DownloadRequest(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.profileId = source["profileId"];
	        this.bucket = source["bucket"];
	        this.region = source["region"];
	        this.keys = source["keys"];
	        this.destDir = source["destDir"];
	    }
	}
	export class JobView {
	    id: string;
	    direction: string;
	    profile: string;
	    bucket: string;
	    key: string;
	    localPath: string;
	    name: string;
	    state: string;
	    phase: string;
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
	
	    static createFrom(source: any = {}) {
	        return new JobView(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.direction = source["direction"];
	        this.profile = source["profile"];
	        this.bucket = source["bucket"];
	        this.key = source["key"];
	        this.localPath = source["localPath"];
	        this.name = source["name"];
	        this.state = source["state"];
	        this.phase = source["phase"];
	        this.totalBytes = source["totalBytes"];
	        this.doneBytes = source["doneBytes"];
	        this.bytesPerSec = source["bytesPerSec"];
	        this.etaSeconds = source["etaSeconds"];
	        this.partsDone = source["partsDone"];
	        this.partsTotal = source["partsTotal"];
	        this.retries = source["retries"];
	        this.reason = source["reason"];
	        this.errorCode = source["errorCode"];
	        this.detail = source["detail"];
	        this.warning = source["warning"];
	        this.finished = source["finished"];
	        this.canPause = source["canPause"];
	        this.canResume = source["canResume"];
	        this.canCancel = source["canCancel"];
	        this.canRetry = source["canRetry"];
	        this.canRemove = source["canRemove"];
	    }
	}
	export class Refusal {
	    path: string;
	    code: string;
	    reason: string;
	
	    static createFrom(source: any = {}) {
	        return new Refusal(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.path = source["path"];
	        this.code = source["code"];
	        this.reason = source["reason"];
	    }
	}
	export class SettingsLimits {
	    minParallelFiles: number;
	    maxParallelFiles: number;
	    minPartsPerFile: number;
	    maxPartsPerFile: number;
	    minTotalParts: number;
	    maxTotalParts: number;
	    minPartSize: number;
	    maxPartSize: number;
	
	    static createFrom(source: any = {}) {
	        return new SettingsLimits(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.minParallelFiles = source["minParallelFiles"];
	        this.maxParallelFiles = source["maxParallelFiles"];
	        this.minPartsPerFile = source["minPartsPerFile"];
	        this.maxPartsPerFile = source["maxPartsPerFile"];
	        this.minTotalParts = source["minTotalParts"];
	        this.maxTotalParts = source["maxTotalParts"];
	        this.minPartSize = source["minPartSize"];
	        this.maxPartSize = source["maxPartSize"];
	    }
	}
	export class TransferSettings {
	    parallelFiles: number;
	    partsPerFile: number;
	    totalParts: number;
	    partSize: number;
	    uploadBytesPerSec: number;
	    downloadBytesPerSec: number;
	
	    static createFrom(source: any = {}) {
	        return new TransferSettings(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.parallelFiles = source["parallelFiles"];
	        this.partsPerFile = source["partsPerFile"];
	        this.totalParts = source["totalParts"];
	        this.partSize = source["partSize"];
	        this.uploadBytesPerSec = source["uploadBytesPerSec"];
	        this.downloadBytesPerSec = source["downloadBytesPerSec"];
	    }
	}
	export class SettingsState {
	    current: TransferSettings;
	    defaults: TransferSettings;
	    limits: SettingsLimits;
	
	    static createFrom(source: any = {}) {
	        return new SettingsState(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.current = this.convertValues(source["current"], TransferSettings);
	        this.defaults = this.convertValues(source["defaults"], TransferSettings);
	        this.limits = this.convertValues(source["limits"], SettingsLimits);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class Status {
	    available: boolean;
	    error?: string;
	    uploadsDisabled: boolean;
	    uploadsDisabledReason?: string;
	    warnings: string[];
	    cleanup: CleanupStatus;
	
	    static createFrom(source: any = {}) {
	        return new Status(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.available = source["available"];
	        this.error = source["error"];
	        this.uploadsDisabled = source["uploadsDisabled"];
	        this.uploadsDisabledReason = source["uploadsDisabledReason"];
	        this.warnings = source["warnings"];
	        this.cleanup = this.convertValues(source["cleanup"], CleanupStatus);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class SubmitItem {
	    source: string;
	    kind: string;
	    jobId?: string;
	    error?: string;
	    code?: string;
	    queued: number;
	    jobIds?: string[];
	    refused?: Refusal[];
	    refusedTotal: number;
	    examined: number;
	    emptyFolders?: number;
	    placeholders?: number;
	
	    static createFrom(source: any = {}) {
	        return new SubmitItem(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.source = source["source"];
	        this.kind = source["kind"];
	        this.jobId = source["jobId"];
	        this.error = source["error"];
	        this.code = source["code"];
	        this.queued = source["queued"];
	        this.jobIds = source["jobIds"];
	        this.refused = this.convertValues(source["refused"], Refusal);
	        this.refusedTotal = source["refusedTotal"];
	        this.examined = source["examined"];
	        this.emptyFolders = source["emptyFolders"];
	        this.placeholders = source["placeholders"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class SubmitReport {
	    requested: number;
	    queued: number;
	    refused: Refusal[];
	    refusedTotal: number;
	    emptyFolders: number;
	    placeholders: number;
	    examined: number;
	
	    static createFrom(source: any = {}) {
	        return new SubmitReport(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.requested = source["requested"];
	        this.queued = source["queued"];
	        this.refused = this.convertValues(source["refused"], Refusal);
	        this.refusedTotal = source["refusedTotal"];
	        this.emptyFolders = source["emptyFolders"];
	        this.placeholders = source["placeholders"];
	        this.examined = source["examined"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class SubmitResult {
	    items: SubmitItem[];
	
	    static createFrom(source: any = {}) {
	        return new SubmitResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.items = this.convertValues(source["items"], SubmitItem);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	
	export class UploadRequest {
	    profileId: string;
	    bucket: string;
	    region: string;
	    prefix: string;
	    paths: string[];
	
	    static createFrom(source: any = {}) {
	        return new UploadRequest(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.profileId = source["profileId"];
	        this.bucket = source["bucket"];
	        this.region = source["region"];
	        this.prefix = source["prefix"];
	        this.paths = source["paths"];
	    }
	}

}


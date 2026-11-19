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
	    jobId?: string;
	    error?: string;
	
	    static createFrom(source: any = {}) {
	        return new SubmitItem(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.source = source["source"];
	        this.jobId = source["jobId"];
	        this.error = source["error"];
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


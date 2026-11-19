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


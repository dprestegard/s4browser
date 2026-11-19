export namespace main {
	
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


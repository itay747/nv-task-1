const keys = (process.env.API_KEYS || "").split(",").map(k => k.trim());
db = db.getSiblingDB(process.env.MONGO_INITDB_DATABASE || "infra");
if (keys.length) {
    db.api_keys.insertMany(keys.map(key => ({ key })));
}
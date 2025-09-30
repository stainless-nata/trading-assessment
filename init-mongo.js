db = db.getSiblingDB('trading_api');

db.api_keys.createIndex({ "key": 1 }, { unique: true });

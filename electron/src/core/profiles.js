'use strict';
const fs = require('node:fs');
const path = require('node:path');
const { createHash } = require('node:crypto');

// Only cluster names select profiles. Key rotation within a cluster keeps identity.
function profileDir(dataDir, cluster) {
  return path.join(dataDir, 'profiles', createHash('sha256').update(cluster, 'utf8').digest('hex'));
}
function initializeProfiles(dataDir, cluster) {
  fs.mkdirSync(dataDir, { recursive: true });
  const marker = path.join(dataDir, 'legacy-profile.json');
  if (!fs.existsSync(marker)) fs.writeFileSync(marker, JSON.stringify({ cluster }), { flag: 'wx' });
  const owner = JSON.parse(fs.readFileSync(marker, 'utf8')).cluster;
  const dir = profileDir(dataDir, cluster);
  fs.mkdirSync(dir, { recursive: true });
  const migrated = path.join(dir, 'legacy-migrated');
  if (owner === cluster && !fs.existsSync(migrated)) {
    for (const name of ['node.json', 'node.json.wal', 'bootstrap.json']) {
      const from = path.join(dataDir, name), to = path.join(dir, name);
      if (fs.existsSync(from) && !fs.existsSync(to)) fs.copyFileSync(from, to, fs.constants.COPYFILE_EXCL);
    }
  }
  fs.writeFileSync(migrated, 'done');
  return dir;
}
function previewProfile(dataDir, from, destination, currentEntries) {
  const file = path.join(profileDir(dataDir, destination), 'node.json');
  let stored = null;
  if (fs.existsSync(file)) stored = JSON.parse(fs.readFileSync(file, 'utf8'));
  return { from, destination, currentEntries, destinationEntries: fs.existsSync(file+'.wal') ? null : Object.keys(stored?.entries || {}).length,
    existing: !!stored, policy: 'Keep source data and identity in its profile. Load the destination profile, or start a new identity. No data is copied; use Export and Import explicitly to migrate.' };
}
module.exports = { profileDir, initializeProfiles, previewProfile };

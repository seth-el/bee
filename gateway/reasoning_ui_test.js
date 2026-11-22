const fs = require('fs');
const source = fs.readFileSync('gateway/index.html', 'utf8');
const start = source.indexOf('function addReasoningEvent');
const end = source.indexOf('function formatArtifactSize', start);
if (start < 0 || end < 0) throw new Error('reasoning helper missing');
const helper = source.slice(start, end);
for (const marker of ['data-reasoning-key', 'textContent', 'details', 'incomplete']) {
  if (!helper.includes(marker)) throw new Error(`reasoning helper missing ${marker}`);
}
if (helper.includes('innerHTML')) throw new Error('reasoning helper uses innerHTML');
if (!helper.includes('!text && payload.complete && !payload.incomplete && !payload.error_class')) throw new Error('empty completed reasoning panels are not suppressed');
if (!source.includes("case 'reasoning_delta'") || !source.includes("case 'reasoning_final'")) throw new Error('reasoning run handlers missing');
console.log('reasoning UI helper tests: PASS');

package bee

import "time"

// memoryMatchesTemporal applies the bounded M5 temporal/currentness contract.
// An as-of query is conservative: the current stored version must not have
// been created or updated after the requested timestamp. Full historical
// version reconstruction remains a later event-replay enhancement.
func memoryMatchesTemporal(record MemoryRecord, query MemoryQuery, asOf time.Time) bool {
	if !query.AsOf.IsZero() {
		if record.CreatedAt.After(asOf) || record.UpdatedAt.After(asOf) {
			return false
		}
	}
	if !query.Since.IsZero() && record.UpdatedAt.Before(query.Since) {
		return false
	}
	if !query.Before.IsZero() && !record.UpdatedAt.Before(query.Before) {
		return false
	}
	if query.CurrentOnly && record.Status.effective() != MemoryStatusActive {
		return false
	}
	return true
}

func memoryCurrentAt(record MemoryRecord, asOf time.Time) bool {
	return record.Status.effective() == MemoryStatusActive && !isMemoryExpired(record, DefaultMemoryPolicy(), asOf) && !record.CreatedAt.After(asOf) && !record.UpdatedAt.After(asOf)
}

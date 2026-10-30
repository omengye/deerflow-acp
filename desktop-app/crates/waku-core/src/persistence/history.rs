use super::*;

/// Keep only operation identities when a fork result is deleted. Its complete
/// transcript must be removed, while delayed retries must not recreate it.
pub(super) fn invalidate_deleted_history_result(
    transaction: &rusqlite::Transaction<'_>,
    deleted_id: &str,
) -> anyhow::Result<()> {
    let rows = {
        let mut statement = transaction.prepare("SELECT session_id, data FROM session_history_operations WHERE json_extract(data,'$.result.id')=?1 AND session_id<>?1")?;
        statement
            .query_map([deleted_id], |row| {
                Ok((row.get::<_, String>(0)?, row.get::<_, String>(1)?))
            })?
            .collect::<Result<Vec<_>, _>>()?
    };
    for (source, data) in rows {
        let data: serde_json::Value = serde_json::from_str(&data)?;
        let mut ids = Vec::new();
        if let Some(id) = data["request"]["operationId"].as_str() {
            ids.push(id.to_owned());
        }
        for field in ["operation_aliases", "previous_operation_ids"] {
            if let Some(values) = data[field].as_array() {
                ids.extend(
                    values
                        .iter()
                        .filter_map(|value| value.as_str().map(str::to_owned)),
                );
            }
        }
        let receipt =
            serde_json::json!({"completed":true,"deleted_result":true,"operation_ids":ids});
        transaction.execute(
            "UPDATE session_history_operations SET data=?1 WHERE session_id=?2",
            params![receipt.to_string(), source],
        )?;
    }
    Ok(())
}

impl StateStore {
    pub(crate) fn has_pending_history(&self, session_id: Uuid) -> anyhow::Result<bool> {
        let connection = self.open()?;
        Ok(connection.query_row(
            "SELECT EXISTS(SELECT 1 FROM session_history_operations WHERE json_extract(data,'$.completed') IS NOT 1 AND (session_id=?1 OR json_extract(data,'$.result.id')=?1))",
            params![session_id.to_string()], |row| row.get(0),
        )?)
    }

    pub(crate) fn pending_history_operations(&self) -> anyhow::Result<Vec<serde_json::Value>> {
        let connection = self.open()?;
        let mut statement = connection.prepare("SELECT data FROM session_history_operations WHERE json_extract(data,'$.completed') IS NOT 1")?;
        let rows = statement.query_map([], |row| row.get::<_, String>(0))?;
        rows.map(|row| Ok(serde_json::from_str(&row?)?)).collect()
    }

    pub(crate) fn history_operation(
        &self,
        session_id: Uuid,
    ) -> anyhow::Result<Option<serde_json::Value>> {
        let connection = self.open()?;
        let data: Option<String> = connection
            .query_row(
                "SELECT data FROM session_history_operations WHERE session_id=?1",
                params![session_id.to_string()],
                |row| row.get(0),
            )
            .optional()?;
        data.map(|data| serde_json::from_str(&data).map_err(Into::into))
            .transpose()
    }

    #[cfg(test)]
    pub(crate) fn history_operations(&self) -> anyhow::Result<Vec<(Uuid, serde_json::Value)>> {
        let connection = self.open()?;
        let mut statement =
            connection.prepare("SELECT session_id, data FROM session_history_operations")?;
        let rows = statement.query_map([], |row| {
            Ok((row.get::<_, String>(0)?, row.get::<_, String>(1)?))
        })?;
        rows.map(|row| {
            let (id, data) = row?;
            Ok((Uuid::parse_str(&id)?, serde_json::from_str(&data)?))
        })
        .collect()
    }

    pub(crate) fn save_history_operation(
        &self,
        session_id: Uuid,
        data: &serde_json::Value,
    ) -> anyhow::Result<()> {
        let connection = self.open()?;
        connection.execute_batch("PRAGMA synchronous = FULL;")?;
        connection.execute(
            "INSERT INTO session_history_operations(session_id, data) VALUES (?1, ?2) ON CONFLICT(session_id) DO UPDATE SET data=excluded.data",
            params![session_id.to_string(), serde_json::to_string(data)?],
        )?;
        Ok(())
    }
}

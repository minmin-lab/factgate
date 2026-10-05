SELECT row_id, event_timestamp, processed_at FROM final_v5_result_heavy WHERE processed_at <= event_timestamp AND NOT (processed_at = event_timestamp) ORDER BY row_id

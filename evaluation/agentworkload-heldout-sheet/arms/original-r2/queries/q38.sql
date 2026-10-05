SELECT expense_type, COUNT(*) AS receipt_count, SUM(amount) / COUNT(*) AS avg_amount FROM expense_detail GROUP BY expense_type ORDER BY expense_type

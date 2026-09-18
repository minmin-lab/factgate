SELECT expense_type, count(*) AS receipt_count, sum(amount) / count(*) AS avg_amount FROM expense_detail GROUP BY expense_type ORDER BY expense_type;

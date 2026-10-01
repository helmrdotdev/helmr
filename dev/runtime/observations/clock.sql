SELECT jsonb_build_object('epoch', extract(epoch FROM clock_timestamp()));
